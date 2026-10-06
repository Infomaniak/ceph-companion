package ceph

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// LsblkDevice holds identity info for a local block device.
type LsblkDevice struct {
	Serial string
	WWN    string
}

// SmartpqiEvent tracks the kernel device name(s) a SmartPQI (HPE RAID
// controller) WWN was last seen removed/added as, correlated from kernel
// log events. A controller can cycle a WWN through more than one device
// name across a remove/re-add, which is what ResolveDevice (in the operator
// package) uses this for.
type SmartpqiEvent struct {
	Removed []string
	Added   []string
}

// CephDeviceInfo holds the serial/WWN/OSD-ID mapping for one device as
// reported by `cephadm shell ceph device ls-by-host`.
type CephDeviceInfo struct {
	Serial string
	WWN    string
	OSDIDs []string
}

// normalizeWWN lowercases a WWN and strips the "0x" prefix some tools emit,
// so WWNs read from lsblk, ceph, and journalctl compare equal.
func normalizeWWN(wwn string) string {
	return strings.ReplaceAll(strings.ToLower(wwn), "0x", "")
}

// parseLsblkJSON parses `lsblk -d -J -o NAME,SERIAL,WWN` output into a
// device-name -> {serial, wwn} map.
func parseLsblkJSON(raw []byte) (map[string]LsblkDevice, error) {
	var parsed struct {
		BlockDevices []struct {
			Name   string `json:"name"`
			Serial string `json:"serial"`
			WWN    string `json:"wwn"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	mapping := make(map[string]LsblkDevice, len(parsed.BlockDevices))
	for _, dev := range parsed.BlockDevices {
		if dev.Name == "" {
			continue
		}
		mapping[dev.Name] = LsblkDevice{Serial: dev.Serial, WWN: normalizeWWN(dev.WWN)}
	}
	return mapping, nil
}

// ParseLsblkJSON parses lsblk output into a device-name -> {serial, wwn}
// map, returning an empty map if the output can't be parsed (callers that
// treat missing identity info as "unknown device").
func ParseLsblkJSON(raw []byte) map[string]LsblkDevice {
	mapping, _ := parseLsblkJSON(raw)
	return mapping
}

// LsblkMapping runs `lsblk -d -J -o NAME,SERIAL,WWN` and returns each local
// block device's serial number and WWN.
func (c *Client) LsblkMapping() (map[string]LsblkDevice, error) {
	out, err := ExecOutput("lsblk", "-d", "-J", "-o", "NAME,SERIAL,WWN")
	if err != nil {
		return nil, fmt.Errorf("lsblk failed: %w", err)
	}
	mapping, err := parseLsblkJSON(out)
	if err != nil {
		return nil, fmt.Errorf("failed to parse lsblk JSON: %w", err)
	}
	return mapping, nil
}

var (
	smartpqiSingleRe  = regexp.MustCompile(`(?i)smartpqi.*\b(removed|added)\s+(\d+:\d+:\d+:\d+)\s+([0-9a-fA-F]{32,})\b.*\b(sd[a-z]+)\b`)
	smartpqiWWNOnlyRe = regexp.MustCompile(`(?i)smartpqi.*\b(removed|added)\s+(\d+:\d+:\d+:\d+)\s+([0-9a-fA-F]{32,})\b`)
)

// ParseSmartpqiEvents scans journalctl kernel-log output for SmartPQI (HPE
// RAID controller) device add/remove events, correlating each WWN with the
// kernel device name(s) it was seen as.
func ParseSmartpqiEvents(raw string) map[string]SmartpqiEvent {
	events := make(map[string]SmartpqiEvent)
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if m := smartpqiSingleRe.FindStringSubmatch(line); m != nil {
			wwn := normalizeWWN(m[3])
			ev := events[wwn]
			if strings.EqualFold(m[1], "removed") {
				ev.Removed = append(ev.Removed, m[4])
			} else {
				ev.Added = append(ev.Added, m[4])
			}
			events[wwn] = ev
			continue
		}
		if m := smartpqiWWNOnlyRe.FindStringSubmatch(line); m != nil {
			wwn := normalizeWWN(m[3])
			if _, ok := events[wwn]; !ok {
				events[wwn] = SmartpqiEvent{}
			}
		}
	}
	return events
}

// SmartpqiEvents runs journalctl over the last `minutes` and returns SmartPQI
// device add/remove correlations (see ParseSmartpqiEvents).
func (c *Client) SmartpqiEvents(minutes int) (map[string]SmartpqiEvent, error) {
	out, err := ExecOutput("journalctl", "-k", "--since", fmt.Sprintf("%d minutes ago", minutes), "--no-pager")
	if err != nil {
		return nil, fmt.Errorf("journalctl failed: %w", err)
	}
	return ParseSmartpqiEvents(string(out)), nil
}

var osdIDRe = regexp.MustCompile(`osd\.(\d+)`)

// ParseCephDeviceList parses `cephadm shell ceph device ls-by-host <host>`
// output into a device-name -> {serial, osd_ids} map. It doesn't resolve
// WWNs (that needs a filesystem/lsblk lookup per device) - see
// Client.CephDeviceMapping, which fills that in.
func ParseCephDeviceList(raw string) map[string]CephDeviceInfo {
	deviceMap := make(map[string]CephDeviceInfo)

	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "DEVICE") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		deviceName := parts[1]
		matches := osdIDRe.FindAllStringSubmatch(strings.Join(parts[2:], " "), -1)
		if len(matches) == 0 {
			continue
		}
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m[1]
		}

		serial := ""
		if idx := strings.LastIndex(parts[0], "_"); idx >= 0 {
			serial = parts[0][idx+1:]
		}

		deviceMap[deviceName] = CephDeviceInfo{Serial: serial, OSDIDs: ids}
	}
	return deviceMap
}

// CephDeviceMapping runs `cephadm shell ceph device ls-by-host <hostname>`
// and, for each listed device that exists locally, looks up its WWN via
// lsblk. Returns WWN/serial -> OSD-ID maps plus a per-device info map.
func (c *Client) CephDeviceMapping(hostname string) (wwnMap, serialMap map[string][]string, deviceMap map[string]CephDeviceInfo, err error) {
	out, runErr := ExecOutput("cephadm", "shell", "ceph", "device", "ls-by-host", hostname)
	if runErr != nil {
		return nil, nil, nil, fmt.Errorf("ceph device ls-by-host failed: %w", runErr)
	}

	deviceMap = ParseCephDeviceList(string(out))
	wwnMap = make(map[string][]string)
	serialMap = make(map[string][]string)

	for name, info := range deviceMap {
		if _, statErr := os.Stat("/dev/" + name); statErr == nil {
			if wwnOut, wwnErr := ExecOutput("lsblk", "-d", "-n", "-o", "WWN", "/dev/"+name); wwnErr == nil {
				if wwn := normalizeWWN(strings.TrimSpace(string(wwnOut))); wwn != "" {
					info.WWN = wwn
					deviceMap[name] = info
				}
			}
		}

		if info.WWN != "" {
			wwnMap[info.WWN] = append(wwnMap[info.WWN], info.OSDIDs...)
		}
		if info.Serial != "" {
			serialMap[info.Serial] = append(serialMap[info.Serial], info.OSDIDs...)
		}
	}

	return wwnMap, serialMap, deviceMap, nil
}

var (
	nvmeBaseRe       = regexp.MustCompile(`^(nvme\d+n\d+)`)
	trailingDigitsRe = regexp.MustCompile(`\d+$`)
)

// BaseDeviceName strips a partition suffix from a kernel device name to get
// the whole-disk name (e.g. "sda1" -> "sda", "nvme0n1p2" -> "nvme0n1") -
// the form attribution (lsblk/ceph device tables, /sys/block) keys on.
func BaseDeviceName(device string) string {
	if strings.HasPrefix(device, "nvme") {
		if m := nvmeBaseRe.FindStringSubmatch(device); m != nil {
			return m[1]
		}
		return device
	}
	return trailingDigitsRe.ReplaceAllString(device, "")
}

// IsRotational reports whether device is a spinning disk (HDD) rather than
// flash (SSD/NVMe), read from /sys/block. A device whose rotational flag
// can't be read is conservatively treated as rotational - safer to assume
// HDD (and let a hardware-error rule consider it) than to silently skip a
// real disk error because of a sysfs read failure.
func (c *Client) IsRotational(device string) bool {
	base := BaseDeviceName(device)
	data, err := os.ReadFile(fmt.Sprintf("/sys/block/%s/queue/rotational", base))
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(data)) == "1"
}
