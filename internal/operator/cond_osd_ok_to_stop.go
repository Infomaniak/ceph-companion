package operator

import (
	"fmt"
)

// Command: osd_ok_to_stop. Given OSDs from a prior chained condition
// (input_items), checks each is safe to stop and not already stopped.
// Triggers only if all are safe.

// evalOSDOkToStop checks if OSDs are safe to stop.
func (e *Engine) evalOSDOkToStop(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	inputItems, ok := kwargs["input_items"].([]Item)
	if !ok || len(inputItems) == 0 {
		return &EvalResult{Triggered: false}
	}

	var osds []string
	for _, it := range inputItems {
		if it.Type == "osd" {
			osds = append(osds, it.ID)
		}
	}

	if len(osds) == 0 {
		return &EvalResult{Triggered: false}
	}

	var results []Item
	var logs []string
	var errs []string

	for _, osdID := range osds {
		stopped, err := e.CephClient.OSDStopped(osdID)
		if err != nil {
			// Without cephadm we can't even tell whether the OSD is
			// running - surface it instead of pretending "stopped".
			line := fmt.Sprintf("osd.%s: stopped-state check failed: %v", osdID, err)
			errs = append(errs, line)
			logs = append(logs, line)
			continue
		}
		if stopped {
			results = append(results, Item{
				Type:       "osd",
				ID:         osdID,
				FullName:   fmt.Sprintf("osd.%s", osdID),
				SafeToStop: false,
			})
			logs = append(logs, fmt.Sprintf("osd.%s: already stopped", osdID))
			continue
		}

		safe, err := e.CephClient.OSDOkToStop(osdID)
		results = append(results, Item{
			Type:       "osd",
			ID:         osdID,
			FullName:   fmt.Sprintf("osd.%s", osdID),
			SafeToStop: safe && err == nil,
		})

		if err != nil {
			// A failed check must never be silently indistinguishable from
			// "not safe": a missing cephadm binary would otherwise blind
			// every rule that ends in an OSD stop.
			line := fmt.Sprintf("osd.%s: ok-to-stop check failed: %v", osdID, err)
			errs = append(errs, line)
			logs = append(logs, line)
			continue
		}
		if safe {
			logs = append(logs, fmt.Sprintf("osd.%s: safe", osdID))
		} else {
			logs = append(logs, fmt.Sprintf("osd.%s: NOT safe", osdID))
		}
	}

	triggered := len(results) > 0
	for _, r := range results {
		if !r.SafeToStop {
			triggered = false
			break
		}
	}

	return &EvalResult{Items: results, Triggered: triggered, Logs: logs, Errors: errs}
}
