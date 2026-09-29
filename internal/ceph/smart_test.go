package ceph

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The user-reported SAS error-log sample: four bad sectors auto-reassigned
// by the drive ([4,9,0] = HARDWARE ERROR / vendor specific).
const sasSample = `smartctl 7.2 2020-12-30 r5155 [x86_64-linux-5.10.0]
Vendor:               SEAGATE
Product:              ST8000NM0075
Serial number:        ZC123XYZ
Elements in grown defect list: 4
Error counter log:
           7        0        0   0   0   0   0       0         0
  1 3227:28  0000000072cf4000  [4,9,0]   Successfully reassigned
  2 3227:28  0000000072cf4108  [4,9,0]   Successfully reassigned
  3 3227:28  0000000072cf4150  [4,9,0]   Successfully reassigned
  4 4824:04  00000002136b63e0  [4,9,0]   Successfully reassigned`

func TestParseSmartMediaSAS(t *testing.T) {
	report := ParseSmartMedia("sdw", sasSample)

	if report.ErrorLogCount != 4 {
		t.Errorf("expected 4 error-log entries (sk=4), got %d", report.ErrorLogCount)
	}
	if report.GrownDefects != 4 {
		t.Errorf("expected 4 grown defects, got %d", report.GrownDefects)
	}
	if report.Total != 4 {
		t.Errorf("expected Total=4 (error log and grown defects reflect the same events, not summed), got %d", report.Total)
	}
	if report.Model != "SEAGATE ST8000NM0075" {
		t.Errorf("expected vendor+product model, got %q", report.Model)
	}
	if report.Serial != "ZC123XYZ" {
		t.Errorf("expected serial, got %q", report.Serial)
	}
}

func TestParseSmartMediaATA(t *testing.T) {
	ataSample := `Device Model:     HGST HUS728T8TALE6L4
Serial Number:    ZR123ABC
  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       8
197 Current_Pending_Sector  0x0022   100   100   000    Old_age   Always       -       2
198 Offline_Uncorrectable   0x0008   100   100   010    Old_age   Offline      -       1
199 UDMA_CRC_Error_Count    0x003e   200   200   000    Old_age   Always       -       0`

	report := ParseSmartMedia("sdw", ataSample)

	if report.Total != 11 { // 8 + 2 + 1, CRC errors are not media errors
		t.Errorf("expected Total=11 (reallocated+pending+uncorrectable), got %d", report.Total)
	}
	if report.Model != "HGST HUS728T8TALE6L4" {
		t.Errorf("expected ATA model, got %q", report.Model)
	}
	if report.Serial != "ZR123ABC" {
		t.Errorf("expected serial, got %q", report.Serial)
	}
}

func TestParseSmartMediaTransportErrorsNotCounted(t *testing.T) {
	// sk=0 (no sense) and sk=1 (recovered) entries are transport-level, not
	// failing media.
	transport := `  1 12:00  0000000000abc000  [0,9,0]   No reassignment needed
  2 13:00  0000000000abc008  [1,9,0]   No reassignment needed`

	report := ParseSmartMedia("sdw", transport)
	if report.ErrorLogCount != 0 || report.Total != 0 {
		t.Errorf("expected transport-level entries to be ignored, got total=%d log=%d", report.Total, report.ErrorLogCount)
	}
}

func TestParseSmartMediaGrownDefectsFallback(t *testing.T) {
	// SAS drives can report a grown defect list with an empty error log.
	grown := "Elements in grown defect list: 12"
	report := ParseSmartMedia("sdw", grown)
	if report.Total != 12 {
		t.Errorf("expected Total=12 from grown defect list, got %d", report.Total)
	}
}

// TestParseSmartMediaPendingDefects replays the smartctl -x output of a
// genuinely failing Seagate ST24000NM007H (cephosd-34, /dev/sdw):
// 56 pending defects and 7 unrecovered read errors ([3,11,0], MEDIUM ERROR)
// pending reassignment - all invisible to smartctl -a, which the parser fed
// on before, so the drive counted as "0 media error(s)".
func TestParseSmartMediaPendingDefects(t *testing.T) {
	pendingSample := `Vendor:               SEAGATE
Product:              ST24000NM007H
Serial number:        TESTSER05
SMART Health Status: OK
Elements in grown defect list: 0
Non-medium error count:        0
  Pending defect count:56 Pending Defects: index, LBA and accumulated_power_on_hours follow
     1:  0xefaf7808        ,   5554
     2:  0xefaf7809        ,   5554
Background scan results log
  Status: no scans active
   #  when        lba(hex)    [sk,asc,ascq]    reassign_status
   1 4650:30  00000000eed2ee30  [1,17,1]   Recovered via rewrite in-place
   2 4663:33  00000000f7def2d8  [1,17,1]   Recovered via rewrite in-place
   3 4663:34  00000000f7def2f8  [1,17,1]   Recovered via rewrite in-place
  19 4753:34  00000001678591a0  [1,17,1]   Recovered via rewrite in-place
  20 5554:16  00000000efaf7808  [3,11,0]   Require Write or Reassign Blocks command
  21 5554:16  00000000efaf7858  [3,11,0]   Require Write or Reassign Blocks command
  26 5554:16  00000000efaf7a90  [3,11,0]   Require Write or Reassign Blocks command`

	report := ParseSmartMedia("sdw", pendingSample)

	if report.ErrorLogCount != 3 {
		t.Errorf("expected 3 error-log entries ([3,11,0] MEDIUM ERROR), got %d", report.ErrorLogCount)
	}
	if report.GrownDefects != 0 {
		t.Errorf("expected 0 grown defects, got %d", report.GrownDefects)
	}
	if report.PendingDefects != 56 {
		t.Errorf("expected 56 pending defects, got %d", report.PendingDefects)
	}
	if report.RecoveredRewrites != 4 {
		t.Errorf("expected 4 recovered rewrite-in-place entries (informational), got %d", report.RecoveredRewrites)
	}
	if report.Total != 56 {
		// max(error-log=7, grown=0, pending=56): the [3,11,0] entries share
		// their LBAs with 7 of the 56 pending defects, so max dedups them
		// instead of counting one bad sector twice.
		t.Errorf("expected Total=56 (largest of error-log/grown/pending), got %d", report.Total)
	}
	if !strings.Contains(report.Summary, "pending-defects=56") {
		t.Errorf("expected the summary to expose pending defects, got %q", report.Summary)
	}
	if !strings.Contains(report.Summary, "recovered-rewrites=4") {
		t.Errorf("expected the summary to expose recovered rewrites, got %q", report.Summary)
	}
}

func TestOSDMetadataDevices(t *testing.T) {
	tests := []struct {
		name     string
		json     string // printed by the cephadm stub
		expected []string
		wantErr  bool
	}{
		{
			name:     "object form",
			json:     `{"id": 205, "hostname": "gva2a-object-cephosd-2", "devices": "nvme3n1,sdw"}`,
			expected: []string{"nvme3n1", "sdw"},
		},
		{
			name:     "array form",
			json:     `[{"id": 205, "devices": "sdb"}]`,
			expected: []string{"sdb"},
		},
		{
			name:    "no devices field",
			json:    `{"id": 205}`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := filepath.Join(dir, "cephadm")
			script := fmt.Sprintf("#!/bin/sh\ncat <<'EOFJSON'\n%s\nEOFJSON\n", test.json)
			if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))

			devices, err := NewClient().OSDMetadataDevices("205")
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(devices, test.expected) {
				t.Errorf("devices = %v, want %v", devices, test.expected)
			}
		})
	}
}

func TestSmartMediaErrorsParsesDespiteNonZeroExit(t *testing.T) {
	// smartctl exits non-zero when health is FAILED (bitmask), but still
	// prints the data - the report must be parsed anyway.
	dir := t.TempDir()
	stub := filepath.Join(dir, "smartctl")
	script := `#!/bin/sh
echo "Elements in grown defect list: 3"
echo "  1 100:00  0000000000aa0000  [4,9,0]   Successfully reassigned"
exit 4
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	report, err := NewClient().SmartMediaErrors("sdw")
	if err != nil {
		t.Fatalf("expected data despite non-zero exit, got error: %v", err)
	}
	if report.Total != 3 {
		t.Errorf("expected Total=3 (max of error-log=1, grown=3), got %d", report.Total)
	}
}

func TestSmartMediaErrorsMissingBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // empty PATH: no smartctl anywhere

	if _, err := NewClient().SmartMediaErrors("sdw"); err == nil {
		t.Fatal("expected an error when smartctl is not installed")
	}
}
