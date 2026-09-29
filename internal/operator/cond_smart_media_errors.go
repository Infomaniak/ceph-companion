package operator

import (
	"fmt"
)

// Command: smart_media_errors. For OSDs flagged by an earlier condition
// (input_items), resolves the OSD's backing devices via `ceph osd metadata`
// (e.g. "nvme3n1,sdw"), keeps only the rotational ones - SMART media errors
// on the data HDD matter here; the NVMe/SSD accelerator is intentionally
// skipped - and counts media-error indicators in each device's SMART data
// (SCSI/SAS error-log entries like "[4,9,0] Successfully reassigned", grown
// defect lists, ATA reallocated/pending/uncorrectable sectors). Triggers
// when an OSD's worst device reaches error_limit.

// filterRotational partitions devices into spinning disks and flash,
// with isRot injected for testability.
func filterRotational(devices []string, isRot func(string) bool) (hdds, skipped []string) {
	for _, d := range devices {
		if isRot(d) {
			hdds = append(hdds, d)
		} else {
			skipped = append(skipped, d)
		}
	}
	return hdds, skipped
}

func (e *Engine) evalSmartMediaErrors(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	errorLimit := 1
	if cond.ErrorLimit != nil {
		errorLimit = *cond.ErrorLimit
	}

	inputItems, ok := kwargs["input_items"].([]Item)
	if !ok || len(inputItems) == 0 {
		return &EvalResult{Triggered: false, Logs: []string{"smart_media_errors requires an earlier condition that flags OSDs (input_items)"}}
	}

	var logs []string
	var errs []string
	var items []Item
	for _, it := range inputItems {
		if it.Type != "osd" || it.ID == "" {
			continue
		}

		devices, err := e.CephClient.OSDMetadataDevices(it.ID)
		if err != nil {
			line := fmt.Sprintf("osd.%s: cannot resolve devices: %v", it.ID, err)
			logs = append(logs, line)
			errs = append(errs, line)
			continue
		}
		hdds, skipped := filterRotational(devices, e.CephClient.IsRotational)
		for _, dev := range skipped {
			logs = append(logs, fmt.Sprintf("osd.%s: skipped /dev/%s (non-rotational)", it.ID, dev))
		}
		if len(hdds) == 0 {
			logs = append(logs, fmt.Sprintf("osd.%s: no rotational device behind OSD - nothing to check", it.ID))
			continue
		}

		worst := 0
		for _, dev := range hdds {
			report, err := e.CephClient.SmartMediaErrors(dev)
			if err != nil {
				line := fmt.Sprintf("osd.%s: /dev/%s: %v", it.ID, dev, err)
				logs = append(logs, line)
				errs = append(errs, line)
				continue
			}
			if report.Model != "" {
				logs = append(logs, fmt.Sprintf("osd.%s: /dev/%s (%s): %d media error(s) - %s",
					it.ID, dev, report.Model, report.Total, report.Summary))
			} else {
				logs = append(logs, fmt.Sprintf("osd.%s: /dev/%s: %d media error(s) - %s",
					it.ID, dev, report.Total, report.Summary))
			}
			if report.Total > worst {
				worst = report.Total
			}
		}

		if worst >= errorLimit {
			items = append(items, Item{
				Type:       "osd",
				ID:         it.ID,
				FullName:   fmt.Sprintf("osd.%s", it.ID),
				ErrorCount: worst,
			})
		}
	}

	if len(items) > 0 {
		logs = append(logs, fmt.Sprintf("Found %d OSD(s) with SMART media errors", len(items)))
	}
	return &EvalResult{Items: items, Triggered: len(items) > 0, Logs: logs, Errors: errs}
}
