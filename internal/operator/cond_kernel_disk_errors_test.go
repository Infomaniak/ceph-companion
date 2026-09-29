package operator

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/ceph"
)

// incidentKernelLog replays the journal lines of both observed false
// positives: LVM's pvs probing the zero-capacity ghost gendisk (sdd) of a
// replaced disk (cephosd-11, 2026-09-08), and ceph-volume
// probing the ghost (sdt) of a replaced disk whose rebuilt OSD reuses the
// name (cephosd-19, 2026-09-14).
const incidentKernelLog = `Sep 08 07:54:29 cephosd-11 kernel: bio_check_eod: 17 callbacks suppressed
Sep 08 07:54:29 cephosd-11 kernel: pvs: attempt to access beyond end of device
                                                sdd: rw=0, sector=34816, nr_sectors = 256 limit=0
Sep 08 07:54:30 cephosd-11 kernel: pvs: attempt to access beyond end of device
                                                sdd: rw=0, sector=34816, nr_sectors = 256 limit=0
Sep 08 07:54:30 cephosd-11 kernel: pvs: attempt to access beyond end of device
                                                sdd: rw=0, sector=34816, nr_sectors = 256 limit=0
Sep 13 09:58:22 cephosd-19 kernel: ceph-volume: attempt to access beyond end of device
                                             sdt: rw=524288, sector=2048, nr_sectors = 32 limit=0
Sep 13 09:58:22 cephosd-19 kernel: ceph-volume: attempt to access beyond end of device
                                             sdt: rw=524288, sector=2048, nr_sectors = 32 limit=0
Sep 08 06:47:15 cephosd-11 kernel: sd 0:0:25:0: [sdy] Attached SCSI disk
`

func TestProcessKernelLinesPhantomProbesSkipped(t *testing.T) {
	scan := processKernelLines(strings.Split(incidentKernelLog, "\n"))

	// Probing a zero-capacity (limit=0) device is not a medium error: the
	// replaced disk's ghost must not count against the rebuilt OSD that
	// now owns the device name.
	if scan.counts["sdd"] != 0 {
		t.Errorf("expected 0 counted hits for ghost sdd, got %d", scan.counts["sdd"])
	}
	if _, ok := scan.counts["sdy"]; ok {
		t.Errorf("expected no hits for sdy (re-attach line names no error), got %v", scan.counts)
	}
	if len(scan.samples["sdd"]) != 0 {
		t.Errorf("expected no real samples for phantom hits, got %v", scan.samples["sdd"])
	}
	if len(scan.phantomSamples["sdd"]) != 3 {
		t.Errorf("expected 3 phantom samples for sdd, got %v", scan.phantomSamples["sdd"])
	}
	if scan.phantomCounts["sdd"] != 3 {
		t.Errorf("expected 3 phantom (zero-capacity) hits for sdd, got %d", scan.phantomCounts["sdd"])
	}
}

// TestProcessKernelLinesBeyondEndWithRealLimit keeps the context-attribution
// path covered: a "beyond end of device" hit against a device the kernel
// sees with a real (non-zero) size is a real hit - only limit=0 hits are
// bucketed as phantoms.
func TestProcessKernelLinesBeyondEndWithRealLimit(t *testing.T) {
	lines := []string{
		"Sep 13 09:58:22 host kernel: ceph-volume: attempt to access beyond end of device",
		"                                                sdt: rw=524288, sector=2048, nr_sectors = 32 limit=247565",
	}
	scan := processKernelLines(lines)

	if scan.counts["sdt"] != 1 {
		t.Errorf("expected 1 counted hit for sdt (limit is non-zero), got %d", scan.counts["sdt"])
	}
	if scan.phantomCounts["sdt"] != 0 {
		t.Errorf("expected no phantom hits, got %d", scan.phantomCounts["sdt"])
	}
	if len(scan.samples["sdt"]) != 1 || !strings.Contains(scan.samples["sdt"][0], "limit=247565") {
		t.Errorf("expected a joined keyword|detail sample, got %v", scan.samples["sdt"])
	}
}

func TestProcessKernelLinesSingleLinePhantom(t *testing.T) {
	// Device name and limit=0 on the keyword line itself (merged journal
	// lines): bucketed as a phantom probe.
	lines := []string{
		"sdt: attempt to access beyond end of device, sector 2048 limit=0",
	}
	scan := processKernelLines(lines)
	if scan.counts["sdt"] != 0 {
		t.Errorf("expected single-line phantom not to count, got %d", scan.counts["sdt"])
	}
	if scan.phantomCounts["sdt"] != 1 {
		t.Errorf("expected the single-line phantom in the phantom bucket, got %d", scan.phantomCounts["sdt"])
	}
}

func TestProcessKernelLinesDirectKeywordAndSampleCap(t *testing.T) {
	lines := []string{
		"blk_update_request: I/O error, dev sda, sector 1",
		"blk_update_request: I/O error, dev sda, sector 2",
		"blk_update_request: I/O error, dev sda, sector 3",
		"blk_update_request: I/O error, dev sda, sector 4",
		"blk_update_request: I/O error, dev sda, sector 5",
		// The detail line must not double-count: the keyword line above
		// already named the device.
		"sda: rw=0, sector=2, limit=100",
	}
	scan := processKernelLines(lines)

	if scan.counts["sda"] != 5 {
		t.Errorf("expected 5 hits for sda (detail line skipped, already named), got %d", scan.counts["sda"])
	}
	if len(scan.samples["sda"]) != maxSamplesPerDevice {
		t.Errorf("expected samples capped at %d, got %d", maxSamplesPerDevice, len(scan.samples["sda"]))
	}
	if !strings.Contains(scan.samples["sda"][0], "sector 1") {
		t.Errorf("expected first raw error line as sample, got %q", scan.samples["sda"][0])
	}
}

// renamedDiskKernelLog is the incident log variant with a non-zero device
// limit: "beyond end of device" against a device the kernel still sizes
// counts as an error, unlike the limit=0 phantom probes. Used to verify the
// legit-rename attribution path still works for real hits.
const renamedDiskKernelLog = `Sep 08 07:54:29 cephosd-11 kernel: bio_check_eod: 17 callbacks suppressed
Sep 08 07:54:29 cephosd-11 kernel: pvs: attempt to access beyond end of device
                                                sdd: rw=0, sector=34816, nr_sectors = 256 limit=247565
Sep 08 07:54:30 cephosd-11 kernel: pvs: attempt to access beyond end of device
                                                sdd: rw=0, sector=34816, nr_sectors = 256 limit=247565
Sep 08 06:47:15 cephosd-11 kernel: sd 0:0:25:0: [sdy] Attached SCSI disk
`

// installKernelErrorStubs puts fake `journalctl` (replays the incident
// kernel log), `lsblk` (sdy + nvme4n1, no sdd - it's a ghost) and
// `cephadm` (device table) on PATH.
func installKernelErrorStubs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()

	journalctl := "#!/bin/sh\ncat <<'EOF'\n" + incidentKernelLog + "EOF\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(journalctl), 0o755); err != nil {
		t.Fatal(err)
	}

	// lsblk serves both call shapes: the -J full mapping and the per-device
	// `-d -n -o WWN /dev/<name>` lookup done by CephDeviceMapping.
	lsblk := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-J" ]; then
    cat <<'EOF'
{"blockdevices":[
  {"name":"sdy","serial":"TESTSER02","wwn":"0x5000c50000000002"},
  {"name":"nvme4n1","serial":"TESTSER04","wwn":"0xe8238fa60000000f"}
]}
EOF
    exit 0
  fi
done
for arg in "$@"; do
  case "$arg" in
    /dev/sdy)     echo "0x5000c50000000002"; exit 0 ;;
    /dev/nvme4n1) echo "0xe8238fa60000000f"; exit 0 ;;
  esac
done
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "lsblk"), []byte(lsblk), 0o755); err != nil {
		t.Fatal(err)
	}

	cephadm := `#!/bin/sh
cat <<'EOF'
DEVICE                                DEV      DAEMONS
SEAGATE_ST24000NM007H_TESTSER02        sdy      osd.51
SEAGATE_ST24000NM007H_TESTSER03        sdt      osd.850
MTFDKCC3T8TGP-1BK1DABYY_TESTSER04  nvme4n1  osd.23 osd.30 osd.42 osd.51 osd.62 osd.73 osd.84 osd.96
EOF
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "cephadm"), []byte(cephadm), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installRenamedDiskStubs is the legit-rename variant: the disk that once
// was sdd now reports as sdy in ceph (same serial), but its WWN is absent
// from lsblk output in this run - so the cache entry keeps
// last_known_device=sdd and the name-only fallback is what attributes the
// ghost-name errors back to the same physical disk's OSD.
func installRenamedDiskStubs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()

	journalctl := "#!/bin/sh\ncat <<'EOF'\n" + renamedDiskKernelLog + "EOF\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(journalctl), 0o755); err != nil {
		t.Fatal(err)
	}

	lsblk := `#!/bin/sh
cat <<'EOF'
{"blockdevices":[
  {"name":"nvme4n1","serial":"TESTSER04","wwn":"0xe8238fa60000000f"}
]}
EOF
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "lsblk"), []byte(lsblk), 0o755); err != nil {
		t.Fatal(err)
	}

	cephadm := `#!/bin/sh
cat <<'EOF'
DEVICE                          DEV  DAEMONS
SEAGATE_ST24000NM007H_TESTSER01  sdy  osd.51
EOF
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "cephadm"), []byte(cephadm), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeDeviceCacheFile(t *testing.T, cache DeviceCache) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device_history.json")
	if err := saveDeviceCache(path, cache); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEvalKernelLogsGhostDeviceNotAttributed replays both incidents end to
// end:
//   - stale cache: the stale cache entry (replaced disk TESTSER01, last
//     known as sdd) claims osd.51, but live data moved osd.51 to TESTSER02 -
//     reconciliation invalidates it before attribution.
//   - ghost device: the replaced disk TESTSER03's ghost (sdt) is still claimed by
//     the ceph device table (sdt -> osd.850), but the cache's record of
//     that identity carries no osd_ids, so the ownership can't be
//     corroborated and the zero-capacity probes count against nothing.
//
// Either way no OSD is flagged, and every refusal is journalled.
func TestEvalKernelLogsGhostDeviceNotAttributed(t *testing.T) {
	installKernelErrorStubs(t)
	cachePath := writeDeviceCacheFile(t, DeviceCache{
		"5000c50000000001": {Serial: "TESTSER01", LastKnownDevice: "sdd", OSDIDs: []string{"51"}},
		// ghost-device state: the cache saw the (new) disk in lsblk on Sep 8 but
		// ceph never resolved its WWN, so no osd_ids were ever recorded.
		"5000c50000000003": {Serial: "TESTSER03", LastKnownDevice: "sdt"},
	})

	eng := &Engine{CephClient: ceph.NewClient(), DeviceCachePath: cachePath}
	minutes, limit := 1440, 2
	cond := &RuleCondition{Minutes: &minutes, ErrorLimit: &limit, RotationalOnly: true}

	result := eng.evalKernelLogs(nil, cond)

	if result.Triggered {
		t.Errorf("expected no trigger from ghost-device errors, items: %v, logs: %v", result.Items, result.Logs)
	}
	if len(result.Items) != 0 {
		t.Errorf("expected no items, got %v", result.Items)
	}
	// The limit=0 probes must be visibly ignored, not silently dropped.
	joined := strings.Join(result.Logs, "\n")
	if !strings.Contains(joined, "ignored") || !strings.Contains(joined, "zero-capacity probe hit(s)") {
		t.Errorf("expected the ignored-phantom explanation in logs, got: %s", joined)
	}
	if !strings.Contains(joined, "5") {
		t.Errorf("expected all 5 phantom hits (3 sdd + 2 sdt) in the ignored count, got: %s", joined)
	}
	if len(result.AlwaysLog) != 1 {
		t.Fatalf("expected exactly one AlwaysLog invalidation line, got %v", result.AlwaysLog)
	}
	for _, want := range []string{"TESTSER01", "sdd", "osd.51", "TESTSER02"} {
		if !strings.Contains(result.AlwaysLog[0], want) {
			t.Errorf("invalidation line %q should mention %q", result.AlwaysLog[0], want)
		}
	}

	// The invalidated entry must be gone from the persisted cache.
	reloaded := loadDeviceCache(cachePath)
	if _, ok := reloaded["5000c50000000001"]; ok {
		t.Errorf("expected stale entry purged from cache file, got %v", reloaded)
	}
}

// TestEvalKernelLogsPhantomOnVanishedDiskTriggers is the protective
// counterpart the cache exists for: the disk crashes and vanishes from
// lsblk entirely, but the cache still records that its identity
// (TESTSER03/sdt) owns osd.850 - a first-time drop, not a replacement - so
// the zero-capacity probes must count and the rule must trigger.
func TestEvalKernelLogsPhantomOnVanishedDiskTriggers(t *testing.T) {
	installKernelErrorStubs(t)
	// Same cache as the ghost test, except the identity's osd_ids record
	// osd.850: the cache corroborates the ceph table's claim.
	cachePath := writeDeviceCacheFile(t, DeviceCache{
		"5000c50000000003": {Serial: "TESTSER03", LastKnownDevice: "sdt", OSDIDs: []string{"850"}},
	})

	eng := &Engine{CephClient: ceph.NewClient(), DeviceCachePath: cachePath}
	minutes, limit := 1440, 2
	cond := &RuleCondition{Minutes: &minutes, ErrorLimit: &limit, RotationalOnly: true}

	result := eng.evalKernelLogs(nil, cond)

	if !result.Triggered {
		t.Fatalf("expected trigger for the corroborated crashed disk, logs: %v", result.Logs)
	}
	found := false
	for _, it := range result.Items {
		if it.ID == "850" && it.KernelDevice == "sdt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an osd.850 item on sdt, got %v", result.Items)
	}
	joined := strings.Join(result.Logs, "\n")
	if !strings.Contains(joined, "zero-capacity (limit=0) hit(s)") || !strings.Contains(joined, "corroborated") {
		t.Errorf("evidence should note the corroborated zero-capacity hits, got: %s", joined)
	}
}

// TestEvalKernelLogsPhantomOnLSBlkVisibleDiskTriggers: the disk drops to
// zero capacity but stays visible in lsblk with its identity - the
// strongest form of "this is the OSD's live disk". The lsblk-serial
// attribution alone promotes the zero-capacity hits.
func TestEvalKernelLogsPhantomOnLSBlkVisibleDiskTriggers(t *testing.T) {
	dir := t.TempDir()

	journalctl := `#!/bin/sh
cat <<'EOF'
Sep 13 09:58:22 host kernel: ceph-volume: attempt to access beyond end of device
                                             sdt: rw=524288, sector=2048, nr_sectors = 32 limit=0
Sep 13 09:58:22 host kernel: ceph-volume: attempt to access beyond end of device
                                             sdt: rw=524288, sector=2048, nr_sectors = 32 limit=0
EOF
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(journalctl), 0o755); err != nil {
		t.Fatal(err)
	}
	lsblk := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-J" ]; then
    cat <<'EOF'
{"blockdevices":[
  {"name":"sdt","serial":"TESTSER03","wwn":"0x5000c50000000003"}
]}
EOF
    exit 0
  fi
done
for arg in "$@"; do
  case "$arg" in
    /dev/sdt) echo "0x5000c50000000003"; exit 0 ;;
  esac
done
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "lsblk"), []byte(lsblk), 0o755); err != nil {
		t.Fatal(err)
	}
	cephadm := `#!/bin/sh
cat <<'EOF'
DEVICE                          DEV  DAEMONS
SEAGATE_ST24000NM007H_TESTSER03  sdt  osd.850
EOF
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "cephadm"), []byte(cephadm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cachePath := writeDeviceCacheFile(t, DeviceCache{
		"5000c50000000003": {Serial: "TESTSER03", LastKnownDevice: "sdt", OSDIDs: []string{"850"}},
	})

	eng := &Engine{CephClient: ceph.NewClient(), DeviceCachePath: cachePath}
	minutes, limit := 1440, 2
	cond := &RuleCondition{Minutes: &minutes, ErrorLimit: &limit, RotationalOnly: true}

	result := eng.evalKernelLogs(nil, cond)

	if !result.Triggered {
		t.Fatalf("expected trigger for the zero-capacity live disk, logs: %v", result.Logs)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "850" {
		t.Errorf("expected osd.850 item, got %v", result.Items)
	}
	joined := strings.Join(result.Logs, "\n")
	if !strings.Contains(joined, "via lsblk-serial") {
		t.Errorf("expected lsblk-serial attribution, got: %s", joined)
	}
}

// TestEvalKernelLogsRenamedDiskStillAttributed is the legit counterpart:
// the disk was renamed (sdd -> sdy) but is the same physical device, so the
// ghost-name errors still attribute to its OSD - with the attribution
// source and sample kernel lines in the journalled evidence.
func TestEvalKernelLogsRenamedDiskStillAttributed(t *testing.T) {
	installRenamedDiskStubs(t)
	// Same serial as the live ceph mapping -> reconciliation keeps the
	// entry; the lsblk pass can't refresh last_known_device (WWN absent
	// from lsblk in this run), so sdd still resolves via the cache.
	cachePath := writeDeviceCacheFile(t, DeviceCache{
		"5000c50000000001": {Serial: "TESTSER01", LastKnownDevice: "sdd", OSDIDs: []string{"51"}},
	})

	eng := &Engine{CephClient: ceph.NewClient(), DeviceCachePath: cachePath}
	minutes, limit := 1440, 2
	cond := &RuleCondition{Minutes: &minutes, ErrorLimit: &limit, RotationalOnly: true}

	result := eng.evalKernelLogs(nil, cond)

	if !result.Triggered {
		t.Fatalf("expected trigger for the renamed-but-same disk, logs: %v", result.Logs)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "51" {
		t.Errorf("expected osd.51 item, got %v", result.Items)
	}
	if len(result.AlwaysLog) != 0 {
		t.Errorf("expected no cache invalidations, got %v", result.AlwaysLog)
	}
	joined := strings.Join(result.Logs, "\n")
	for _, want := range []string{"kernel error hit(s)", "via", "limit=247565", "osd.51"} {
		if !strings.Contains(joined, want) {
			t.Errorf("evidence logs should mention %q, got: %s", want, joined)
		}
	}
}

func TestExecActionLivePrintsTrail(t *testing.T) {
	eng := &Engine{LiveMode: true, CephClient: ceph.NewClient()}
	rule := &Rule{
		Name: "trail",
		Action: RuleAction{
			Command:        "echo osd-{id}-ran",
			MaxItemsPerRun: 1,
		},
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	eng.execAction(rule, []Item{{Type: "osd", ID: "51", FullName: "osd.51"}})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), "[Exec]") {
		t.Errorf("expected live mode to print the executed command, got %q", string(out))
	}
	if !strings.Contains(string(out), "osd-51-ran") {
		t.Errorf("expected live mode to print the command output, got %q", string(out))
	}
}

// A failing chain must surface stderr - that is where the actual reason
// lives ("sh: ceph: command not found" for a bare `ceph orch` on a cephadm
// host), while stdout may hold noise from the earlier && stages.
func TestExecActionFailShowsStderr(t *testing.T) {
	eng := &Engine{LiveMode: true, CephClient: ceph.NewClient()}
	rule := &Rule{
		Name: "fail",
		Action: RuleAction{
			Command:        "echo pre-step-ok && /nonexistent/binary-{id}",
			MaxItemsPerRun: 1,
		},
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	ok, actionErrs := eng.execAction(rule, []Item{{Type: "osd", ID: "950", FullName: "osd.950"}})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if ok {
		t.Error("expected execAction to return false for a failing command")
	}
	if len(actionErrs) != 1 || !strings.Contains(actionErrs[0], "fail / action: command failed on osd.950") {
		t.Errorf("expected one batched action-error line, got %v", actionErrs)
	}
	if !strings.Contains(string(out), "FAIL") {
		t.Errorf("expected FAIL status line, got %q", string(out))
	}
	if !strings.Contains(string(out), "nonexistent") {
		t.Errorf("expected stderr reason to be shown, got %q", string(out))
	}
	// The FAIL line must start with stderr, not with the noise from the
	// earlier && stages ("FAIL: pre-step-ok..." would be misleading).
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "FAIL") && !strings.Contains(line, "[Exec]") {
			if strings.Contains(line, "pre-step-ok") {
				t.Errorf("FAIL line should show stderr, got stdout noise: %q", line)
			}
		}
	}
}

// Every `cephadm shell` stage of a failing &&-chain adds stderr noise
// ("Inferring fsid ..."), and the real error only comes last. The FAIL
// line must not truncate inside that preamble.
func TestExecActionFailFiltersCephadmStderr(t *testing.T) {
	eng := &Engine{LiveMode: true, CephClient: ceph.NewClient()}
	rule := &Rule{
		Name: "cephadm-noise",
		Action: RuleAction{
			Command:        "echo Inferring fsid abc-123 >&2 && echo Using ceph image xyz >&2 && echo quay.io/ceph/ceph@sha256:abc >&2 && /nonexistent/binary-{id}",
			MaxItemsPerRun: 1,
		},
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	eng.execAction(rule, []Item{{Type: "osd", ID: "950", FullName: "osd.950"}})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	var failLine string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "FAIL") && !strings.Contains(line, "[Exec]") {
			failLine = line
		}
	}
	if failLine == "" {
		t.Fatalf("expected a FAIL status line, got %q", string(out))
	}
	if !strings.Contains(failLine, "nonexistent") {
		t.Errorf("expected the real error to surface in the FAIL line, got %q", failLine)
	}
	if strings.Contains(failLine, "Inferring") || strings.Contains(failLine, "Using ceph image") {
		t.Errorf("expected cephadm preamble to be filtered from the FAIL line, got %q", failLine)
	}
}

func TestPrintDoneReportsFailedActions(t *testing.T) {
	eng := &Engine{}
	tests := []struct {
		processed, failed int
		want              string
	}{
		{1, 0, "Done: 1 action(s) executed."},
		{0, 2, "Done: 2 action(s) failed."},
		{0, 0, "Done: no action needed."},
	}
	for _, tt := range tests {
		old := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		eng.printDone(3, tt.processed, tt.failed)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		os.Stdout = old
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), tt.want) {
			t.Errorf("printDone(3, %d, %d) = %q, want %q", tt.processed, tt.failed, string(out), tt.want)
		}
	}
}
