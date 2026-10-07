package operator

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/infomaniak/ceph-companion/internal/ceph"
)

// Command: kernel_disk_errors. Scans kernel logs for disk I/O errors, then
// resolves each affected device to the real Ceph OSD ID(s) it backs (see
// device_cache.go for the WWN/serial correlation and persistent cache this
// relies on) instead of treating the raw kernel device name as an OSD ID.

var (
	kernelErrorKeywords = []string{
		"I/O error", "FAILED", "Sense Key", "beyond end of device",
		"blk_update_request", "end_request", "Buffer I/O error",
		"medium error",
	}
	// kernelDeviceRe matches the kernel device names attribution keys on:
	// SATA/SAS disks (sda, sdbb) and NVMe namespaces (nvme0n1) with their
	// partitions (nvme0n1p2, normalized back to the namespace). Controller
	// devices (nvme0) deliberately don't match: controller-level resets and
	// command timeouts say nothing about any namespace's medium.
	kernelDeviceRe  = regexp.MustCompile(`\b(?:sd[a-z]+|nvme\d+n\d+(?:p\d+)?)\b`)
	kernelDevLineRe = regexp.MustCompile(`^\s*(sd[a-z]+|nvme\d+n\d+(?:p\d+)?):`)
	// "attempt to access beyond end of device ... limit=0" means the kernel
	// sees the device with zero capacity: the ghost gendisk of a removed,
	// replaced or bus-dropped disk being probed by pvs, ceph-volume, udev,
	// ... Probing a phantom is expected noise, never a failing medium - a
	// real failure on the same disk additionally logs I/O errors and sense
	// keys, which stay counted. (Both observed false positives - LVM's pvs
	// probing a replaced disk's stale node on cephosd-11 and
	// ceph-volume probing a dropped disk on cephosd-19 - carried
	// limit=0; seen in the samples of the trigger evidence line.)
	beyondEndRe  = regexp.MustCompile(`beyond end of device`)
	ghostLimitRe = regexp.MustCompile(`limit=0\b`)
)

// devicesInLine returns the whole-disk names a kernel line names, with NVMe
// partitions normalized to their namespace ("nvme0n1p2" -> "nvme0n1") - the
// form lsblk, the ceph device table and the cache attribute OSDs by.
func devicesInLine(line string) []string {
	matches := kernelDeviceRe.FindAllString(line, -1)
	devs := make([]string, 0, len(matches))
	for _, m := range matches {
		devs = append(devs, ceph.BaseDeviceName(m))
	}
	return devs
}

// isPhantomProbe reports whether the line itself reports a zero-capacity
// device access (single-line form: device name and limit=0 on one line).
func isPhantomProbe(line string) bool {
	return beyondEndRe.MatchString(line) && ghostLimitRe.MatchString(line)
}

func hasKernelErrorKeyword(line string) bool {
	lower := strings.ToLower(line)
	for _, kw := range kernelErrorKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// Limits on the raw kernel lines kept as evidence per device: enough to
// recognize the failure mode in the journal later, without bloating output
// when a broken device floods the log (kernels rate-limit via "callbacks
// suppressed", but a bad disk can still produce thousands of lines).
const (
	maxSamplesPerDevice = 3
	maxSampleLen        = 200
)

// kernelScan holds what one journalctl scan found. Hits are bucketed by
// whether the kernel saw the device with a real size:
//   - counts/samples: every error-keyword hit, plus "beyond end of device"
//     hits against devices the kernel still sizes (limit>0) - always
//     meaningful.
//   - phantomCounts/phantomSamples: "beyond end of device" hits against
//     zero-capacity devices (limit=0). These are ambiguous: a disk that
//     crashes in place drops to zero capacity (worth the protective OSD
//     stop), but so does the ghost gendisk of a replaced disk being probed
//     by pvs/ceph-volume under a name the rebuilt OSD reuses (worth
//     ignoring). evalKernelLogs resolves the ambiguity by only counting
//     phantom hits for devices whose identity lsblk can verify.
type kernelScan struct {
	counts         map[string]int
	phantomCounts  map[string]int
	samples        map[string][]string
	phantomSamples map[string][]string
}

// scanKernelErrors runs journalctl over the last `minutes` and counts I/O
// error keyword hits per kernel device name (e.g. "sda", "nvme0n1"), keeping
// a few raw sample lines per device for journalled evidence. A hit also
// counts if the device's own log line immediately follows an error line
// within the last few lines of context (disk errors are often logged as a
// keyword line followed by a "sdX: ..." detail line) - zero-capacity hits go
// to their own bucket (see kernelScan).
func scanKernelErrors(minutes int) (*kernelScan, error) {
	since := fmt.Sprintf("%d minutes ago", minutes)
	output, err := ceph.ExecOutput("journalctl", "-k", "--since", since, "--no-pager")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("error reading kernel logs: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("error reading kernel logs: %w", err)
	}

	return processKernelLines(strings.Split(string(output), "\n")), nil
}

// processKernelLines counts I/O-error keyword hits per kernel device and
// collects up to maxSamplesPerDevice raw sample lines per device. Sample
// lines are the raw journal lines (trimmed, truncated to maxSampleLen); for
// context-attributed hits the keyword line is joined with the "<dev>: ..."
// detail line, since the keyword line alone doesn't name the device (e.g.
// "pvs: attempt to access beyond end of device" followed by "sdd: rw=0 ...").
// Hits against zero-capacity devices (limit=0) go to the phantom buckets -
// whether they count is decided at attribution time, once lsblk can verify
// the device's identity (see ghostLimitRe and evalKernelLogs).
func processKernelLines(lines []string) *kernelScan {
	scan := &kernelScan{
		counts:         make(map[string]int),
		phantomCounts:  make(map[string]int),
		samples:        make(map[string][]string),
		phantomSamples: make(map[string][]string),
	}

	addSample := func(target map[string][]string, dev, line string) {
		if len(target[dev]) >= maxSamplesPerDevice {
			return
		}
		if runes := []rune(line); len(runes) > maxSampleLen {
			line = string(runes[:maxSampleLen])
		}
		target[dev] = append(target[dev], line)
	}

	var contextLines [3]string

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if hasKernelErrorKeyword(line) {
			// Zero-capacity access logged on one line (device and limit=0
			// together): bucketed as a phantom probe - see kernelScan.
			target := scan.counts
			sampleTarget := scan.samples
			if isPhantomProbe(line) {
				target = scan.phantomCounts
				sampleTarget = scan.phantomSamples
			}
			for _, dev := range devicesInLine(line) {
				target[dev]++
				addSample(sampleTarget, dev, line)
			}
		}

		if m := kernelDevLineRe.FindStringSubmatch(line); m != nil {
			dev := ceph.BaseDeviceName(m[1])
			if ghostLimitRe.MatchString(line) {
				// A "<dev>: ... limit=0" detail line is the bio_check_eod
				// follow-up for a zero-capacity device: bucket it as a
				// phantom hit when the preceding error line was a "beyond
				// end of device" probe (any other keyword line naming the
				// device directly was already counted in the block above).
				for _, pl := range contextLines {
					if !hasKernelErrorKeyword(pl) || slices.Contains(devicesInLine(pl), dev) {
						continue
					}
					if beyondEndRe.MatchString(pl) {
						scan.phantomCounts[dev]++
						addSample(scan.phantomSamples, dev, pl+" | "+line)
					}
					break
				}
			} else {
				for _, pl := range contextLines {
					if !hasKernelErrorKeyword(pl) {
						continue
					}
					if !slices.Contains(devicesInLine(pl), dev) {
						scan.counts[dev]++
						addSample(scan.samples, dev, pl+" | "+line)
					}
					break
				}
			}
		}

		contextLines[0] = contextLines[1]
		contextLines[1] = contextLines[2]
		contextLines[2] = line
	}

	return scan
}

// Attribution sources returned by findOSDsForDevice, weakest last. The
// source is journalled with every trigger: a "cache-name-only" match for a
// device absent from lsblk is the weakest evidence available and must be
// visibly flagged - it once attributed LVM probes of a ghost gendisk to a
// healthy OSD (replaced disk, stale cache entry) and stopped it.
const (
	SourceLSBlkWWN      = "lsblk-wwn"
	SourceLSBlkSerial   = "lsblk-serial"
	SourceCephDeviceMap = "ceph-device-map"
	SourceCacheWWN      = "cache-wwn"
	SourceCacheSerial   = "cache-serial"
	SourceCacheNameOnly = "cache-name-only"
)

// findOSDsForDevice resolves which OSD ID(s) a disk belongs to, trying (in
// order): the resolved device's WWN, its serial, a direct ceph device-list
// match on the resolved name, then a cache entry matching by WWN, serial, or
// the *original* (unresolved) device name reported in the error. It returns
// the OSD IDs plus the source that matched them, or (nil, "") when nothing
// matches. Unlike the Python source this ported from, an empty serial on
// both sides is not treated as a match - "serial unknown" isn't the same
// fact as "same disk", and this result can feed an automated OSD stop.
func findOSDsForDevice(resolvedDevice, rawDevice string, devInfo ceph.LsblkDevice, wwnMap, serialMap map[string][]string, deviceMap map[string]ceph.CephDeviceInfo, cache DeviceCache) ([]string, string) {
	if devInfo.WWN != "" {
		if osds, ok := wwnMap[devInfo.WWN]; ok {
			return osds, SourceLSBlkWWN
		}
	}
	if devInfo.Serial != "" {
		if osds, ok := serialMap[devInfo.Serial]; ok {
			return osds, SourceLSBlkSerial
		}
	}
	if info, ok := deviceMap[resolvedDevice]; ok {
		return info.OSDIDs, SourceCephDeviceMap
	}
	for wwn, entry := range cache {
		source := ""
		match := false
		if devInfo.WWN != "" && wwn == devInfo.WWN {
			source = SourceCacheWWN
			match = true
		} else if devInfo.Serial != "" && entry.Serial == devInfo.Serial {
			source = SourceCacheSerial
			match = true
		} else if entry.LastKnownDevice == rawDevice {
			source = SourceCacheNameOnly
			match = true
		}
		if match && len(entry.OSDIDs) > 0 {
			return entry.OSDIDs, source
		}
	}
	return nil, ""
}

// evalKernelLogs scans kernel logs for disk I/O errors and resolves each
// affected device to the real Ceph OSD ID(s) it backs, via lsblk/WWN/serial
// correlation against `ceph device ls-by-host` plus a persistent cache
// (device_cache.go) for devices that have since been renamed or removed.
// Zero-capacity phantom hits ("beyond end of device", limit=0) only count
// when the device→OSD ownership is corroborated (live lsblk identity, or a
// cache record that reconciliation didn't contradict) - a crashed disk
// vanishing from lsblk is still attributed through the cache, while the
// ghost gendisk of a replaced disk whose OSD was rebuilt elsewhere is
// ignored. Every trigger journals its evidence (hit counts, sample kernel
// lines, attribution source), and stale cache entries contradicted by live
// data are reconciled away (device_cache.go reconcileDeviceCache) before
// attribution, with the invalidations surfaced via AlwaysLog.
func (e *Engine) evalKernelLogs(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	minutes := 60
	if cond.Minutes != nil {
		minutes = *cond.Minutes
	}
	errorLimit := 1
	if cond.ErrorLimit != nil {
		errorLimit = *cond.ErrorLimit
	}

	var logs []string
	var errs []string

	scan, err := scanKernelErrors(minutes)
	if err != nil {
		// Without the kernel log scan the condition cannot evaluate at all
		// (e.g. journalctl missing) - surface it, don't blend in with a
		// healthy "no errors found" run.
		return &EvalResult{Triggered: false, Errors: []string{err.Error()}}
	}

	lsblkMap, err := e.CephClient.LsblkMapping()
	if err != nil {
		line := fmt.Sprintf("lsblk failed: %v - device attribution degraded (cache/ceph map only)", err)
		logs = append(logs, "Warning: "+line)
		errs = append(errs, line)
		lsblkMap = map[string]ceph.LsblkDevice{}
	}
	smartpqiEvents, err := e.CephClient.SmartpqiEvents(minutes)
	if err != nil {
		// Best-effort probe: it errors on hosts without a SmartPQI
		// controller, which is the normal case - not an execution error.
		smartpqiEvents = map[string]ceph.SmartpqiEvent{}
	}

	hostname, _ := os.Hostname()
	wwnMap, serialMap, deviceMap, err := e.CephClient.CephDeviceMapping(hostname)
	if err != nil {
		line := fmt.Sprintf("ceph device ls-by-host failed: %v - device attribution degraded", err)
		logs = append(logs, "Warning: "+line)
		errs = append(errs, line)
		wwnMap, serialMap, deviceMap = map[string][]string{}, map[string][]string{}, map[string]ceph.CephDeviceInfo{}
	}

	cachePath := e.DeviceCachePath
	if cachePath == "" {
		cachePath = DeviceCacheFile
	}
	cache := loadDeviceCache(cachePath)
	updateDeviceCache(cache, deviceMap, lsblkMap)
	// Reconcile against live data BEFORE attribution: a stale entry (e.g.
	// for a since-replaced disk) would otherwise map ghost-device errors to
	// an OSD that now runs on a different disk.
	invalidations := reconcileDeviceCache(cache, deviceMap, lsblkMap)
	if err := saveDeviceCache(cachePath, cache); err != nil {
		logs = append(logs, fmt.Sprintf("Warning: failed to save device cache: %v", err))
	}

	var items []Item
	var ignoredPhantom int

	// Phantom (limit=0) hits are ambiguous: a disk that crashes in place
	// drops to zero capacity (worth the protective OSD stop), and so does
	// the ghost gendisk of a replaced disk being probed by pvs/ceph-volume
	// under a name the rebuilt OSD reuses (worth ignoring). The gate is
	// cache corroboration, not lsblk presence - a crashed disk often
	// vanishes from lsblk entirely, and the cache exists precisely to keep
	// attributing its errors. Zero-capacity hits count only when the
	// device→OSD link survives scrutiny: attribution through the live
	// lsblk identity or through a cache entry whose osd_ids survived
	// reconciliation (no cache/live mismatch), or, for a ceph-table claim
	// (which can outlive the physical disk), an explicit cache record of
	// that identity owning the OSD.
	devices := make(map[string]struct{}, len(scan.counts)+len(scan.phantomCounts))
	for d := range scan.counts {
		devices[d] = struct{}{}
	}
	for d := range scan.phantomCounts {
		devices[d] = struct{}{}
	}

	for device := range devices {
		resolved := resolveDevice(device, lsblkMap, smartpqiEvents, cache)
		devInfo := lsblkMap[resolved]

		osdIDs, source := findOSDsForDevice(resolved, device, devInfo, wwnMap, serialMap, deviceMap, cache)
		if len(osdIDs) == 0 {
			// Unattributable: its phantom hits are ignored with the rest.
			ignoredPhantom += scan.phantomCounts[device]
			continue
		}
		if cond.RotationalOnly && !e.CephClient.IsRotational(resolved) {
			continue
		}

		count := scan.counts[device]
		phantomHits := scan.phantomCounts[device]
		if phantomHits > 0 && !phantomOwnershipCorroborated(source, deviceMap[resolved], osdIDs, cache) {
			ignoredPhantom += phantomHits
			phantomHits = 0
		}
		count += phantomHits
		if count < errorLimit {
			continue
		}

		var osdNames []string
		for _, osdID := range osdIDs {
			osdNames = append(osdNames, fmt.Sprintf("osd.%s", osdID))
			items = append(items, Item{
				Type:           "osd",
				ID:             osdID,
				FullName:       fmt.Sprintf("osd.%s", osdID),
				KernelDevice:   device,
				ResolvedDevice: resolved,
				ErrorCount:     count,
			})
		}

		evidence := fmt.Sprintf("%d kernel error hit(s) on %q (resolved: %s, via %s) in the last %d min -> %s",
			count, device, resolved, source, minutes, strings.Join(osdNames, ","))
		if phantomHits > 0 {
			evidence += fmt.Sprintf(" [includes %d zero-capacity (limit=0) hit(s) - device ownership corroborated, treating as the OSD's live disk dropping]", phantomHits)
		}
		if s := mergedSamples(scan, device); len(s) > 0 {
			evidence += fmt.Sprintf("; samples: %q", strings.Join(s, " || "))
		}
		logs = append(logs, evidence)
	}

	if ignoredPhantom > 0 {
		logs = append(logs, fmt.Sprintf("ignored %d zero-capacity probe hit(s) (\"beyond end of device\" with limit=0) whose device→OSD ownership the cache cannot corroborate - ghost gendisks of replaced/removed disks probed by pvs/ceph-volume, not the OSD's current disk", ignoredPhantom))
	}

	if len(items) > 0 {
		logs = append(logs, fmt.Sprintf("Found %d OSD(s) with disk errors", len(items)))
	}

	return &EvalResult{
		Items:     items,
		Triggered: len(items) > 0,
		Logs:      logs,
		Errors:    errs,
		AlwaysLog: invalidations,
	}
}

// phantomOwnershipCorroborated decides whether zero-capacity (limit=0) hits
// on a device count against the OSDs attribution produced. Sources that
// verified the device identity against lsblk or against a cache entry whose
// osd_ids just survived reconciliation are trusted as-is. A ceph-table claim
// alone is not enough: `ceph device ls-by-host` keeps listing the old disk
// (and its OSD) after a replacement until the OSD is redeployed, so the
// cache's own record of that identity must agree before the hits count.
func phantomOwnershipCorroborated(source string, info ceph.CephDeviceInfo, osdIDs []string, cache DeviceCache) bool {
	switch source {
	case SourceLSBlkWWN, SourceLSBlkSerial, SourceCacheWWN, SourceCacheSerial, SourceCacheNameOnly:
		return true
	case SourceCephDeviceMap:
		return cacheCorroboratesOwnership(cache, info, osdIDs)
	default:
		return false
	}
}

// cacheCorroboratesOwnership reports whether the cache records every given
// OSD ID as an owner of the identity (serial or WWN) claimed for the device.
func cacheCorroboratesOwnership(cache DeviceCache, info ceph.CephDeviceInfo, osdIDs []string) bool {
	serial, wwn := info.Serial, info.WWN
	if serial == "" && wwn == "" {
		return false
	}
	for key, entry := range cache {
		idMatch := (serial != "" && entry.Serial == serial) || (wwn != "" && key == wwn)
		if !idMatch {
			continue
		}
		all := true
		for _, id := range osdIDs {
			if !slices.Contains(entry.OSDIDs, id) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// mergedSamples joins the real and phantom sample lines for a device,
// each capped at maxSamplesPerDevice at scan time.
func mergedSamples(scan *kernelScan, device string) []string {
	merged := make([]string, 0, len(scan.samples[device])+len(scan.phantomSamples[device]))
	merged = append(merged, scan.samples[device]...)
	merged = append(merged, scan.phantomSamples[device]...)
	return merged
}
