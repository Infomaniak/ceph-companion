package ceph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// OSDMetadata fetches one OSD's metadata from
// `cephadm shell ceph osd metadata osd.<id> --format json`. Ceph versions
// emit either a single object or a one-element array - both are handled.
func (c *Client) OSDMetadata(osdID string) (map[string]interface{}, error) {
	out, err := ExecOutput("cephadm", "shell", "ceph", "osd", "metadata", "osd."+osdID, "--format", "json")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("ceph osd metadata osd.%s failed: %s", osdID, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("ceph osd metadata osd.%s failed: %w", osdID, err)
	}

	blob := bytes.TrimSpace(out)
	var obj map[string]interface{}
	if err := json.Unmarshal(blob, &obj); err != nil {
		var arr []map[string]interface{}
		if err2 := json.Unmarshal(blob, &arr); err2 == nil && len(arr) > 0 {
			return arr[0], nil
		}
		return nil, fmt.Errorf("failed to parse osd metadata JSON: %w", err)
	}
	return obj, nil
}

// OSDMetadataDevices returns the device names backing an OSD, from the
// comma-separated "devices" metadata field (e.g. ["nvme3n1", "sdw"]).
func (c *Client) OSDMetadataDevices(osdID string) ([]string, error) {
	meta, err := c.OSDMetadata(osdID)
	if err != nil {
		return nil, err
	}
	devicesRaw, _ := meta["devices"].(string)
	if devicesRaw == "" {
		return nil, fmt.Errorf("osd metadata for osd.%s has no devices field", osdID)
	}
	var devices []string
	for _, part := range strings.Split(devicesRaw, ",") {
		if d := strings.TrimSpace(part); d != "" {
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("osd metadata for osd.%s lists no devices", osdID)
	}
	return devices, nil
}

// SmartMediaReport holds the media-error indicators found in one device's
// SMART data.
type SmartMediaReport struct {
	Device string
	Model  string
	Serial string
	// SCSI/SAS error-log entries with a medium-error (3) or hardware-error
	// (4) sense key - e.g. "[4,9,0] Successfully reassigned" or
	// "[3,11,0] Require Write or Reassign Blocks command".
	ErrorLogCount int
	// SAS "Elements in grown defect list" (sectors the drive reallocated).
	GrownDefects int
	// SCSI/SAS "Pending defect count" (sectors with unrecovered read errors
	// awaiting a reassign - the SAS equivalent of ATA Current_Pending_Sector).
	PendingDefects int
	// SCSI background-scan entries recovered via "rewrite in-place"
	// (marginal sectors the drive had to rewrite). Informational only -
	// recovered errors are corrected data and healthy drives log them too,
	// so they never count toward Total.
	RecoveredRewrites int
	// ATA attribute values.
	Reallocated   int
	Pending       int
	Uncorrectable int
	// Total is the deduplicated media-error count used against the rule's
	// error_limit: ATA drives count reallocated+pending+uncorrectable
	// sectors; SCSI/SAS drives take the largest of the error-log entries,
	// the grown defect list and the pending defect count (the error-log
	// entries and pending defects share the same bad LBAs - the max dedups
	// them instead of double-counting one sector twice).
	Total   int
	Summary string
}

var (
	// scsiErrorLogRe matches a smartctl SCSI/SAS error-log entry line:
	// "   1 3227:28  0000000072cf4000  [4,9,0]   Successfully reassigned"
	// and captures the sense key (3 = medium error, 4 = hardware error).
	scsiErrorLogRe = regexp.MustCompile(`^\s*\d+\s+.*\[(\d+),\d+,\d+\]`)
	// rewriteInPlaceRe spots the reassign_status of background-scan entries
	// the drive recovered by rewriting the sector in place.
	rewriteInPlaceRe = regexp.MustCompile(`(?i)rewrite in-?place`)
	// pendingDefectRe matches the SCSI/SAS "Pending defect count: N" line
	// (printed by smartctl -x from the background medium-scan log page).
	pendingDefectRe = regexp.MustCompile(`(?i)pending defect count:\s*(\d+)`)
	// ataAttrRe matches ATA SMART attribute rows for the media-error IDs:
	// 5 (Reallocated_Sector_Ct), 197 (Current_Pending_Sector),
	// 198 (Offline_Uncorrectable). RAW_VALUE is the last field.
	ataAttrRe     = regexp.MustCompile(`^\s*(5|197|198)\s+(Reallocated_Sector_Ct|Current_Pending_Sector|Offline_Uncorrectable)\s+.*\s(\d+)\s*$`)
	grownDefectRe = regexp.MustCompile(`(?i)elements in grown defect list:\s*(\d+)`)
	smartSerialRe = regexp.MustCompile(`(?im)^\s*serial (?:number|number \(wwn\)):\s*(.+)$`)
)

// ParseSmartMedia parses `smartctl -a` output and counts media-error
// indicators (see SmartMediaReport).
func ParseSmartMedia(device, raw string) SmartMediaReport {
	report := SmartMediaReport{Device: device}
	vendor, product := "", ""

	for _, line := range strings.Split(raw, "\n") {
		if m := ataAttrRe.FindStringSubmatch(line); m != nil {
			value := 0
			_, _ = fmt.Sscanf(m[3], "%d", &value)
			switch m[2] {
			case "Reallocated_Sector_Ct":
				report.Reallocated = value
			case "Current_Pending_Sector":
				report.Pending = value
			case "Offline_Uncorrectable":
				report.Uncorrectable = value
			}
			continue
		}
		if m := grownDefectRe.FindStringSubmatch(line); m != nil {
			_, _ = fmt.Sscanf(m[1], "%d", &report.GrownDefects)
			continue
		}
		if m := pendingDefectRe.FindStringSubmatch(line); m != nil {
			_, _ = fmt.Sscanf(m[1], "%d", &report.PendingDefects)
			continue
		}
		if m := scsiErrorLogRe.FindStringSubmatch(line); m != nil {
			// Sense key 3 = medium error, 4 = hardware error (e.g. the
			// drive auto-reassigning a bad sector, or an unrecovered read
			// error pending reassignment). Sense key 1 = recovered: the
			// data was corrected, healthy drives log these too - tracked
			// separately (rewrite-in-place entries) and never counted.
			// Transport-level keys don't indicate failing media.
			switch m[1] {
			case "3", "4":
				report.ErrorLogCount++
			case "1":
				if rewriteInPlaceRe.MatchString(line) {
					report.RecoveredRewrites++
				}
			}
			continue
		}
		if report.Model == "" {
			for _, re := range []*regexp.Regexp{
				regexp.MustCompile(`(?im)^\s*device model:\s*(.+)$`),
				regexp.MustCompile(`(?im)^\s*model number:\s*(.+)$`),
			} {
				if m := re.FindStringSubmatch(line); m != nil {
					report.Model = strings.TrimSpace(m[1])
					break
				}
			}
		}
		// SAS drives report identity as separate Vendor/Product lines.
		if vendor == "" {
			if m := regexp.MustCompile(`(?im)^\s*vendor:\s*(.+)$`).FindStringSubmatch(line); m != nil {
				vendor = strings.TrimSpace(m[1])
			}
		}
		if product == "" {
			if m := regexp.MustCompile(`(?im)^\s*product:\s*(.+)$`).FindStringSubmatch(line); m != nil {
				product = strings.TrimSpace(m[1])
			}
		}
		if report.Serial == "" {
			if m := smartSerialRe.FindStringSubmatch(line); m != nil {
				report.Serial = strings.TrimSpace(m[1])
			}
		}
	}
	if report.Model == "" && (vendor != "" || product != "") {
		report.Model = strings.TrimSpace(vendor + " " + product)
	}

	if report.Reallocated > 0 || report.Pending > 0 || report.Uncorrectable > 0 {
		report.Total = report.Reallocated + report.Pending + report.Uncorrectable
		report.Summary = fmt.Sprintf("ATA: reallocated=%d pending=%d uncorrectable=%d",
			report.Reallocated, report.Pending, report.Uncorrectable)
	} else {
		report.Total = report.ErrorLogCount
		if report.GrownDefects > report.Total {
			report.Total = report.GrownDefects
		}
		if report.PendingDefects > report.Total {
			report.Total = report.PendingDefects
		}
		report.Summary = fmt.Sprintf("SCSI: error-log=%d grown-defects=%d pending-defects=%d",
			report.ErrorLogCount, report.GrownDefects, report.PendingDefects)
		if report.RecoveredRewrites > 0 {
			report.Summary += fmt.Sprintf(" recovered-rewrites=%d (informational)", report.RecoveredRewrites)
		}
	}
	return report
}

// SmartMediaErrors runs `smartctl -x /dev/<device>` and parses media-error
// indicators. `-x` (not `-a`) is required for SAS drives: the background
// scan results log (with the "[sk,asc,ascq]" reassignment entries) and the
// pending defect count are only printed there - the exact signals that
// revealed a genuinely failing Seagate ST24000NM007H (56 pending defects,
// 7 unrecovered read errors) as "0 media errors" when only -a was read.
// smartctl's exit code is a bitmask (health-failed, SMART warnings, ...),
// so non-zero exits with usable stdout are still parsed - "health FAILED"
// is exactly the state this report is meant to expose.
func (c *Client) SmartMediaErrors(device string) (SmartMediaReport, error) {
	out, err := ExecOutput("smartctl", "-x", "/dev/"+device)
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok || len(out) == 0 {
			return SmartMediaReport{Device: device}, fmt.Errorf("smartctl for /dev/%s failed (is smartmontools installed?): %w", device, err)
		}
	}
	report := ParseSmartMedia(device, string(out))
	return report, nil
}
