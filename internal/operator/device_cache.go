package operator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/infomaniak/ceph-companion/internal/ceph"
)

// DeviceCacheFile is where device identity history persists across runs, so
// a disk that's since been removed/renamed can still be resolved back to
// its last-known OSD ownership.
const DeviceCacheFile = "/var/cache/ceph-companion/device_history.json"

// DeviceCacheEntry is one WWN's cached identity history.
type DeviceCacheEntry struct {
	Serial          string   `json:"serial"`
	LastKnownDevice string   `json:"last_known_device"`
	OSDIDs          []string `json:"osd_ids,omitempty"`
	Timestamp       string   `json:"timestamp"`
}

// DeviceCache maps a normalized WWN to its cached identity history.
type DeviceCache map[string]DeviceCacheEntry

// loadDeviceCache reads the on-disk cache, returning an empty cache (not an
// error) if it doesn't exist yet or is corrupt.
func loadDeviceCache(path string) DeviceCache {
	data, err := os.ReadFile(path)
	if err != nil {
		return DeviceCache{}
	}
	var cache DeviceCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return DeviceCache{}
	}
	return cache
}

// saveDeviceCache persists the cache, creating its parent directory if needed.
func saveDeviceCache(path string, cache DeviceCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// updateDeviceCache folds fresh ceph-device and lsblk observations into the
// cache. deviceMap (ceph device ls-by-host) contributes OSD ownership;
// lsblkMap (covers every local block device, not just Ceph-managed ones)
// runs second and refreshes each WWN's last-known device name/serial
// without clobbering osd_ids it has no way to know about.
func updateDeviceCache(cache DeviceCache, deviceMap map[string]ceph.CephDeviceInfo, lsblkMap map[string]ceph.LsblkDevice) {
	now := time.Now().UTC().Format(time.RFC3339)

	for devName, info := range deviceMap {
		if info.WWN == "" {
			continue
		}
		cache[info.WWN] = DeviceCacheEntry{
			Serial:          info.Serial,
			LastKnownDevice: devName,
			OSDIDs:          info.OSDIDs,
			Timestamp:       now,
		}
	}

	for devName, info := range lsblkMap {
		if info.WWN == "" {
			continue
		}
		entry := cache[info.WWN]
		entry.Serial = info.Serial
		entry.LastKnownDevice = devName
		entry.Timestamp = now
		cache[info.WWN] = entry
	}
}

// reconcileDeviceCache drops cache entries contradicted by current live
// observations, returning one human-readable explanation per removal (for
// AlwaysLog). The cache exists to carry identity across device renames and
// removals, but a replaced disk leaves a stale entry behind: its ghost
// gendisk keeps logging probe errors under the old name, which the
// name-only fallback (findOSDsForDevice) then attributes to an OSD that now
// lives on a completely different disk. Live data is the ground truth.
//
// Only *positive* conflicts invalidate an entry - missing live data never
// does, so a transient ceph failure can't wipe the cache:
//  1. OSD conflict: the entry claims an OSD that the live ceph device map
//     assigns to other disk(s). An OSD legitimately spans several devices
//     (data disk + NVMe WAL/DB), so the claim is checked against the whole
//     live set of serials/WWNs serving that OSD. Conflicting OSD claims are
//     removed from the entry; the entry itself is deleted once it claims
//     nothing live.
//  2. Name reuse: the entry's last_known_device still exists locally but
//     now carries a different WWN - the name was reassigned to another disk.
func reconcileDeviceCache(cache DeviceCache, deviceMap map[string]ceph.CephDeviceInfo, lsblkMap map[string]ceph.LsblkDevice) []string {
	var removed []string

	// What the live ceph device map says each OSD runs on. Serials come
	// straight from `ceph device ls-by-host`; WWNs only for devices that
	// resolved locally, so serial is the more complete key.
	liveSerialsByOSD := map[string]map[string]bool{}
	liveWWNsByOSD := map[string]map[string]bool{}
	for _, info := range deviceMap {
		for _, osdID := range info.OSDIDs {
			if info.Serial != "" {
				addToSet(liveSerialsByOSD, osdID, info.Serial)
			}
			if info.WWN != "" {
				addToSet(liveWWNsByOSD, osdID, info.WWN)
			}
		}
	}

	for wwn, entry := range cache {
		if len(deviceMap) > 0 {
			var dropped []string
			for _, osdID := range entry.OSDIDs {
				liveSerials := liveSerialsByOSD[osdID]
				liveWWNs := liveWWNsByOSD[osdID]
				if len(liveSerials) == 0 && len(liveWWNs) == 0 {
					continue // live says nothing about this OSD - no conflict provable
				}
				if (entry.Serial != "" && liveSerials[entry.Serial]) || liveWWNs[wwn] {
					continue // claim consistent with live data
				}
				dropped = append(dropped, osdID)
				removed = append(removed, fmt.Sprintf(
					"cache invalidated: wwn %s (serial %s, last_known_device %s) claimed osd.%s, but live ceph mapping assigns osd.%s to: %s",
					wwn, entry.Serial, entry.LastKnownDevice, osdID, osdID, liveDeviceList(deviceMap, osdID)))
			}
			if len(dropped) > 0 {
				entry.OSDIDs = subtract(entry.OSDIDs, dropped)
				if len(entry.OSDIDs) == 0 {
					delete(cache, wwn)
					continue
				}
				cache[wwn] = entry
			}
		}

		// Name reuse: only provable when the current occupant of the name
		// has a known WWN; an empty WWN can't prove the name changed hands.
		if info, ok := lsblkMap[entry.LastKnownDevice]; ok && info.WWN != "" && info.WWN != wwn {
			removed = append(removed, fmt.Sprintf(
				"cache invalidated: device %s now belongs to wwn %s (lsblk), not cached wwn %s (serial %s) - dropping stale entry",
				entry.LastKnownDevice, info.WWN, wwn, entry.Serial))
			delete(cache, wwn)
		}
	}

	return removed
}

// liveDeviceList renders the live devices serving one OSD as sorted
// "name (serial)" pairs for the invalidation log line.
func liveDeviceList(deviceMap map[string]ceph.CephDeviceInfo, osdID string) string {
	var parts []string
	for name, info := range deviceMap {
		if slices.Contains(info.OSDIDs, osdID) {
			parts = append(parts, fmt.Sprintf("%s (%s)", name, info.Serial))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func addToSet(m map[string]map[string]bool, key, value string) {
	if m[key] == nil {
		m[key] = map[string]bool{}
	}
	m[key][value] = true
}

func subtract(slice, drop []string) []string {
	var kept []string
	for _, s := range slice {
		if !slices.Contains(drop, s) {
			kept = append(kept, s)
		}
	}
	return kept
}

// resolveDevice maps a kernel device name reported in an error (e.g. "sda")
// to the device name it should be treated as today:
//  1. It's already a real local device - use it as-is.
//  2. A SmartPQI (HPE RAID controller) event shows its WWN was removed as
//     this name - resolve via that WWN's current lsblk device, or the name
//     it was re-added as.
//  3. The cache's last-known device for some WWN matches - resolve via that
//     WWN's current lsblk device.
//  4. Give up and return the raw name.
func resolveDevice(errorDevice string, lsblkMap map[string]ceph.LsblkDevice, smartpqiEvents map[string]ceph.SmartpqiEvent, cache DeviceCache) string {
	if _, ok := lsblkMap[errorDevice]; ok {
		return errorDevice
	}

	for wwn, evts := range smartpqiEvents {
		if !slices.Contains(evts.Removed, errorDevice) {
			continue
		}
		for name, info := range lsblkMap {
			if info.WWN == wwn {
				return name
			}
		}
		if len(evts.Added) > 0 {
			if _, ok := lsblkMap[evts.Added[0]]; ok {
				return evts.Added[0]
			}
		}
	}

	for wwn, entry := range cache {
		if entry.LastKnownDevice != errorDevice {
			continue
		}
		for name, info := range lsblkMap {
			if info.WWN == wwn {
				return name
			}
		}
	}

	return errorDevice
}
