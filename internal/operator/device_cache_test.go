package operator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/ceph"
)

func TestResolveDeviceDirectMatch(t *testing.T) {
	lsblkMap := map[string]ceph.LsblkDevice{"sda": {WWN: "wwn1"}}
	got := resolveDevice("sda", lsblkMap, nil, DeviceCache{})
	if got != "sda" {
		t.Errorf("expected direct match to return sda, got %q", got)
	}
}

func TestResolveDeviceViaSmartpqiRemovedThenLsblk(t *testing.T) {
	// The device that reported the error ("sda") isn't a real device
	// anymore - the controller re-enumerated the same disk as "sdc".
	lsblkMap := map[string]ceph.LsblkDevice{"sdc": {WWN: "wwn1"}}
	smartpqiEvents := map[string]ceph.SmartpqiEvent{
		"wwn1": {Removed: []string{"sda"}},
	}
	got := resolveDevice("sda", lsblkMap, smartpqiEvents, DeviceCache{})
	if got != "sdc" {
		t.Errorf("expected resolution to sdc via wwn, got %q", got)
	}
}

func TestResolveDeviceViaSmartpqiAddedFallback(t *testing.T) {
	// No lsblk entry carries the wwn, but the controller's "added" event
	// names the device it came back as.
	lsblkMap := map[string]ceph.LsblkDevice{"sdc": {WWN: "other-wwn"}}
	smartpqiEvents := map[string]ceph.SmartpqiEvent{
		"wwn1": {Removed: []string{"sda"}, Added: []string{"sdc"}},
	}
	got := resolveDevice("sda", lsblkMap, smartpqiEvents, DeviceCache{})
	if got != "sdc" {
		t.Errorf("expected resolution to sdc via added fallback, got %q", got)
	}
}

func TestResolveDeviceViaCache(t *testing.T) {
	lsblkMap := map[string]ceph.LsblkDevice{"sdd": {WWN: "wwn2"}}
	cache := DeviceCache{"wwn2": {LastKnownDevice: "sda"}}
	got := resolveDevice("sda", lsblkMap, nil, cache)
	if got != "sdd" {
		t.Errorf("expected resolution to sdd via cache, got %q", got)
	}
}

func TestResolveDeviceGivesUp(t *testing.T) {
	got := resolveDevice("sda", map[string]ceph.LsblkDevice{}, nil, DeviceCache{})
	if got != "sda" {
		t.Errorf("expected raw device name when nothing resolves, got %q", got)
	}
}

func TestUpdateDeviceCachePreservesOSDIDs(t *testing.T) {
	cache := DeviceCache{}
	deviceMap := map[string]ceph.CephDeviceInfo{
		"sda": {Serial: "SER1", WWN: "wwn1", OSDIDs: []string{"5"}},
	}
	lsblkMap := map[string]ceph.LsblkDevice{
		// lsblk sees the disk under a different current name.
		"sdb": {Serial: "SER1", WWN: "wwn1"},
	}

	updateDeviceCache(cache, deviceMap, lsblkMap)

	entry, ok := cache["wwn1"]
	if !ok {
		t.Fatalf("expected cache entry for wwn1, got %v", cache)
	}
	if entry.LastKnownDevice != "sdb" {
		t.Errorf("expected lsblk pass to win last_known_device, got %q", entry.LastKnownDevice)
	}
	if len(entry.OSDIDs) != 1 || entry.OSDIDs[0] != "5" {
		t.Errorf("expected osd_ids from the ceph device pass to survive the lsblk pass, got %v", entry.OSDIDs)
	}
}

func TestFindOSDsForDeviceByWWN(t *testing.T) {
	devInfo := ceph.LsblkDevice{WWN: "wwn1", Serial: "SER1"}
	wwnMap := map[string][]string{"wwn1": {"5", "6"}}
	got, source := findOSDsForDevice("sda", "sda", devInfo, wwnMap, nil, nil, nil)
	if len(got) != 2 || got[0] != "5" || got[1] != "6" {
		t.Errorf("expected osd_ids [5 6] via wwn, got %v", got)
	}
	if source != SourceLSBlkWWN {
		t.Errorf("expected source %q, got %q", SourceLSBlkWWN, source)
	}
}

func TestFindOSDsForDeviceBySerialFallback(t *testing.T) {
	devInfo := ceph.LsblkDevice{Serial: "SER1"} // no WWN known
	serialMap := map[string][]string{"SER1": {"7"}}
	got, source := findOSDsForDevice("sda", "sda", devInfo, nil, serialMap, nil, nil)
	if len(got) != 1 || got[0] != "7" {
		t.Errorf("expected osd_ids [7] via serial, got %v", got)
	}
	if source != SourceLSBlkSerial {
		t.Errorf("expected source %q, got %q", SourceLSBlkSerial, source)
	}
}

func TestFindOSDsForDeviceByDeviceMapFallback(t *testing.T) {
	deviceMap := map[string]ceph.CephDeviceInfo{"sda": {OSDIDs: []string{"8"}}}
	got, source := findOSDsForDevice("sda", "sda", ceph.LsblkDevice{}, nil, nil, deviceMap, nil)
	if len(got) != 1 || got[0] != "8" {
		t.Errorf("expected osd_ids [8] via device map, got %v", got)
	}
	if source != SourceCephDeviceMap {
		t.Errorf("expected source %q, got %q", SourceCephDeviceMap, source)
	}
}

func TestFindOSDsForDeviceByCacheFallback(t *testing.T) {
	cache := DeviceCache{"wwn9": {LastKnownDevice: "sda", OSDIDs: []string{"9"}}}
	got, source := findOSDsForDevice("sda", "sda", ceph.LsblkDevice{}, nil, nil, nil, cache)
	if len(got) != 1 || got[0] != "9" {
		t.Errorf("expected osd_ids [9] via cache last_known_device, got %v", got)
	}
	if source != SourceCacheNameOnly {
		t.Errorf("expected source %q, got %q", SourceCacheNameOnly, source)
	}
}

func TestFindOSDsForDeviceByCacheWWNAndSerial(t *testing.T) {
	// Cache matches by WWN or serial when the device itself isn't in lsblk
	// or the ceph device map - weaker than direct matches, hence labelled.
	cache := DeviceCache{
		"wwn10": {Serial: "SER10", LastKnownDevice: "sdz", OSDIDs: []string{"10"}},
		"wwn11": {Serial: "SER11", LastKnownDevice: "sdy", OSDIDs: []string{"11"}},
	}
	got, source := findOSDsForDevice("sda", "sda", ceph.LsblkDevice{WWN: "wwn10"}, nil, nil, nil, cache)
	if len(got) != 1 || got[0] != "10" || source != SourceCacheWWN {
		t.Errorf("expected osd.10 via cache wwn, got %v (%s)", got, source)
	}
	got, source = findOSDsForDevice("sda", "sda", ceph.LsblkDevice{Serial: "SER11"}, nil, nil, nil, cache)
	if len(got) != 1 || got[0] != "11" || source != SourceCacheSerial {
		t.Errorf("expected osd.11 via cache serial, got %v (%s)", got, source)
	}
}

func TestFindOSDsForDeviceEmptySerialIsNotAMatch(t *testing.T) {
	// Deviation from the Python source: an unknown serial ("") on both
	// sides must not be treated as a match - this result can feed an
	// automated OSD stop, and "serial unknown" isn't evidence of "same disk".
	cache := DeviceCache{"unrelated-wwn": {Serial: "", LastKnownDevice: "sdz", OSDIDs: []string{"99"}}}
	got, source := findOSDsForDevice("sda", "sda", ceph.LsblkDevice{Serial: ""}, nil, nil, nil, cache)
	if len(got) != 0 {
		t.Errorf("expected no match from empty-serial coincidence, got %v", got)
	}
	if source != "" {
		t.Errorf("expected empty source when nothing matches, got %q", source)
	}
}

func TestFindOSDsForDeviceNoMatch(t *testing.T) {
	got, source := findOSDsForDevice("sda", "sda", ceph.LsblkDevice{}, nil, nil, nil, nil)
	if got != nil {
		t.Errorf("expected nil when nothing matches, got %v", got)
	}
	if source != "" {
		t.Errorf("expected empty source when nothing matches, got %q", source)
	}
}

func TestDeviceCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device_history.json")

	cache := DeviceCache{"wwn1": {Serial: "SER1", LastKnownDevice: "sda", OSDIDs: []string{"5"}, Timestamp: "2026-01-01T00:00:00Z"}}
	if err := saveDeviceCache(path, cache); err != nil {
		t.Fatalf("saveDeviceCache failed: %v", err)
	}

	loaded := loadDeviceCache(path)
	if loaded["wwn1"].LastKnownDevice != "sda" {
		t.Errorf("expected round-tripped cache to preserve data, got %v", loaded)
	}

	// Missing file should return an empty cache, not an error.
	empty := loadDeviceCache(filepath.Join(t.TempDir(), "missing.json"))
	if len(empty) != 0 {
		t.Errorf("expected empty cache for missing file, got %v", empty)
	}
}

// TestReconcileDeviceCacheOSDConflict replays the 2026-09-08 incident: a
// replaced disk's stale cache entry (old serial, last known as sdd) still
// claims osd.51, while live ceph data puts osd.51 on different disks.
func TestReconcileDeviceCacheOSDConflict(t *testing.T) {
	cache := DeviceCache{
		// Stale: the removed disk that once hosted osd.51.
		"5000c50000000001": {Serial: "TESTSER01", LastKnownDevice: "sdd", OSDIDs: []string{"51"}},
		// Current: consistent with live data, must survive.
		"5000c50000000002": {Serial: "TESTSER02", LastKnownDevice: "sdy", OSDIDs: []string{"51"}},
	}
	deviceMap := map[string]ceph.CephDeviceInfo{
		"sdy":     {Serial: "TESTSER02", OSDIDs: []string{"51"}},
		"nvme4n1": {Serial: "TESTSER04", OSDIDs: []string{"23", "30", "42", "51", "62", "73", "84", "96"}},
	}
	lsblkMap := map[string]ceph.LsblkDevice{
		// WWNs come normalized (ParseLsblkJSON strips the 0x prefix), same
		// as the cache keys updateDeviceCache writes.
		"sdy":     {Serial: "TESTSER02", WWN: "5000c50000000002"},
		"nvme4n1": {Serial: "TESTSER04", WWN: "e8238fa60000000f"},
		// sdd intentionally absent: it's a ghost gendisk.
	}

	removed := reconcileDeviceCache(cache, deviceMap, lsblkMap)

	if _, ok := cache["5000c50000000001"]; ok {
		t.Errorf("expected stale entry to be removed, cache now: %v", cache)
	}
	if _, ok := cache["5000c50000000002"]; !ok {
		t.Errorf("expected consistent entry to survive, cache now: %v", cache)
	}
	if len(removed) != 1 {
		t.Fatalf("expected exactly one invalidation message, got %v", removed)
	}
	for _, want := range []string{"TESTSER01", "sdd", "osd.51", "TESTSER02"} {
		if !strings.Contains(removed[0], want) {
			t.Errorf("invalidation message %q should mention %q", removed[0], want)
		}
	}
}

func TestReconcileDeviceCacheKeepsMultiDeviceOSDClaim(t *testing.T) {
	// An OSD spans a data disk and an NVMe WAL/DB device; a cache entry
	// naming either one is consistent and must not be invalidated.
	cache := DeviceCache{
		"wwn-hdd":  {Serial: "HDDSER", LastKnownDevice: "sdy", OSDIDs: []string{"51"}},
		"wwn-nvme": {Serial: "NVME1", LastKnownDevice: "nvme4n1", OSDIDs: []string{"51"}},
	}
	deviceMap := map[string]ceph.CephDeviceInfo{
		"sdy":     {Serial: "HDDSER", OSDIDs: []string{"51"}},
		"nvme4n1": {Serial: "NVME1", OSDIDs: []string{"51"}},
	}
	lsblkMap := map[string]ceph.LsblkDevice{
		"sdy":     {Serial: "HDDSER", WWN: "wwn-hdd"},
		"nvme4n1": {Serial: "NVME1", WWN: "wwn-nvme"},
	}

	removed := reconcileDeviceCache(cache, deviceMap, lsblkMap)
	if len(removed) != 0 {
		t.Errorf("expected no invalidations for consistent multi-device OSD, got %v", removed)
	}
	if len(cache) != 2 {
		t.Errorf("expected both entries to survive, cache now: %v", cache)
	}
}

func TestReconcileDeviceCacheTrimsPartialConflict(t *testing.T) {
	// An entry may claim several OSDs (e.g. an NVMe device hosting 8 WALs).
	// One of them now lives on another disk per the live map: drop only that
	// claim, keep the rest.
	cache := DeviceCache{
		"wwn-nvme": {Serial: "NVME1", LastKnownDevice: "nvme4n1", OSDIDs: []string{"23", "30", "999"}},
	}
	deviceMap := map[string]ceph.CephDeviceInfo{
		"nvme4n1": {Serial: "NVME1", OSDIDs: []string{"23", "30"}},
		"sdb":     {Serial: "OTHER", OSDIDs: []string{"999"}},
	}
	lsblkMap := map[string]ceph.LsblkDevice{
		"nvme4n1": {Serial: "NVME1", WWN: "wwn-nvme"},
	}

	removed := reconcileDeviceCache(cache, deviceMap, lsblkMap)
	entry, ok := cache["wwn-nvme"]
	if !ok {
		t.Fatalf("expected entry to survive with trimmed claims, cache now: %v", cache)
	}
	if len(entry.OSDIDs) != 2 {
		t.Errorf("expected osd.999 claim removed, got %v", entry.OSDIDs)
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "osd.999") {
		t.Errorf("expected one invalidation message about osd.999, got %v", removed)
	}
}

func TestReconcileDeviceCacheNameReuse(t *testing.T) {
	// The cached name still exists locally but belongs to another disk now.
	cache := DeviceCache{
		"wwn-old": {Serial: "OLD", LastKnownDevice: "sdd", OSDIDs: []string{"42"}},
	}
	lsblkMap := map[string]ceph.LsblkDevice{"sdd": {Serial: "NEW", WWN: "wwn-new"}}
	// deviceMap empty: the OSD isn't live-mapped anymore, but the name-reuse
	// rule is purely local evidence and still applies.
	removed := reconcileDeviceCache(cache, map[string]ceph.CephDeviceInfo{}, lsblkMap)

	if _, ok := cache["wwn-old"]; ok {
		t.Errorf("expected stale entry to be removed, cache now: %v", cache)
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "wwn-new") {
		t.Errorf("expected invalidation message mentioning the new wwn, got %v", removed)
	}
}

func TestReconcileDeviceCacheKeepsEntryWhenLiveSilent(t *testing.T) {
	// Missing live data (ceph unreachable, lsblk without the device) must
	// never invalidate anything - negative evidence isn't a conflict.
	cache := DeviceCache{
		"wwn-gone": {Serial: "GONE", LastKnownDevice: "sdd", OSDIDs: []string{"51"}},
	}
	removed := reconcileDeviceCache(cache, map[string]ceph.CephDeviceInfo{}, map[string]ceph.LsblkDevice{})
	if len(removed) != 0 {
		t.Errorf("expected no invalidations when live data is empty, got %v", removed)
	}
	if len(cache) != 1 {
		t.Errorf("expected cache to be untouched, cache now: %v", cache)
	}
}

func TestReconcileDeviceCacheKeepsEntryWhenNameWWNUnknown(t *testing.T) {
	// The name still exists in lsblk but with an empty WWN: can't prove the
	// name changed hands, so the entry stays.
	cache := DeviceCache{
		"wwn-x": {Serial: "X", LastKnownDevice: "sdd", OSDIDs: []string{"7"}},
	}
	lsblkMap := map[string]ceph.LsblkDevice{"sdd": {Serial: "Y", WWN: ""}}
	removed := reconcileDeviceCache(cache, map[string]ceph.CephDeviceInfo{}, lsblkMap)
	if len(removed) != 0 {
		t.Errorf("expected no invalidations without provable conflict, got %v", removed)
	}
	if len(cache) != 1 {
		t.Errorf("expected cache to be untouched, cache now: %v", cache)
	}
}
