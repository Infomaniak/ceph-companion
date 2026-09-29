package operator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/ceph"
)

func TestFilterRotational(t *testing.T) {
	hdds, skipped := filterRotational(
		[]string{"nvme3n1", "sdw", "sdc1"},
		func(dev string) bool { return !strings.HasPrefix(dev, "nvme") },
	)
	if len(hdds) != 2 || hdds[0] != "sdw" || hdds[1] != "sdc1" {
		t.Errorf("expected [sdw sdc1], got %v", hdds)
	}
	if len(skipped) != 1 || skipped[0] != "nvme3n1" {
		t.Errorf("expected [nvme3n1] skipped, got %v", skipped)
	}
}

// installOperatorStubs puts fake `cephadm` (returns OSD metadata JSON) and
// `smartctl` (returns the SAS error-log sample) on PATH.
func installOperatorSmartStubs(t *testing.T, smartExit int) {
	t.Helper()
	dir := t.TempDir()

	cephadm := filepath.Join(dir, "cephadm")
	cephadmScript := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "metadata" ]; then
    echo '{"id": "205", "hostname": "test-host", "devices": "nvme3n1,sdw"}'
    exit 0
  fi
done
echo "cephadm unit ok"
exit 0
`
	if err := os.WriteFile(cephadm, []byte(cephadmScript), 0o755); err != nil {
		t.Fatal(err)
	}

	smartctl := filepath.Join(dir, "smartctl")
	smartctlScript := fmt.Sprintf(`#!/bin/sh
cat <<'EOFSMART'
Vendor:               SEAGATE
Product:              ST8000NM0075
Serial number:        ZC123XYZ
Elements in grown defect list: 4
  1 3227:28  0000000072cf4000  [4,9,0]   Successfully reassigned
  2 3227:28  0000000072cf4108  [4,9,0]   Successfully reassigned
  3 3227:28  0000000072cf4150  [4,9,0]   Successfully reassigned
  4 4824:04  00000002136b63e0  [4,9,0]   Successfully reassigned
EOFSMART
exit %d
`, smartExit)
	if err := os.WriteFile(smartctl, []byte(smartctlScript), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestEvalSmartMediaErrorsTriggersOnReassignedSectors(t *testing.T) {
	installOperatorSmartStubs(t, 0)
	eng := &Engine{CephClient: ceph.NewClient()}

	limit := 1
	cond := &RuleCondition{ErrorLimit: &limit}
	kwargs := map[string]interface{}{
		"input_items": []Item{{Type: "osd", ID: "205", FullName: "osd.205"}},
	}

	result := eng.evalSmartMediaErrors(kwargs, cond)
	if !result.Triggered {
		t.Fatalf("expected trigger with 4 reassigned sectors, logs: %v", result.Logs)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "205" || result.Items[0].ErrorCount != 4 {
		t.Errorf("expected one item osd.205 with ErrorCount=4, got %+v", result.Items)
	}
	found := false
	for _, line := range result.Logs {
		if strings.Contains(line, "/dev/sdw") && strings.Contains(line, "4 media error(s)") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a per-device media-error log line, got: %v", result.Logs)
	}
}

func TestEvalSmartMediaErrorsRespectsErrorLimit(t *testing.T) {
	installOperatorSmartStubs(t, 0)
	eng := &Engine{CephClient: ceph.NewClient()}

	limit := 5 // 4 reassigned sectors < 5
	cond := &RuleCondition{ErrorLimit: &limit}
	kwargs := map[string]interface{}{
		"input_items": []Item{{Type: "osd", ID: "205"}},
	}

	result := eng.evalSmartMediaErrors(kwargs, cond)
	if result.Triggered || len(result.Items) != 0 {
		t.Errorf("expected no trigger with error_limit=5 and 4 errors, got %+v", result.Items)
	}
}

func TestEvalSmartMediaErrorsRequiresInputOSDs(t *testing.T) {
	eng := &Engine{CephClient: ceph.NewClient()}
	result := eng.evalSmartMediaErrors(map[string]interface{}{}, &RuleCondition{})
	if result.Triggered {
		t.Error("expected no trigger without input_items")
	}
	for _, line := range result.Logs {
		if strings.Contains(line, "requires an earlier condition") {
			return
		}
	}
	t.Errorf("expected a helpful hint about input_items, got: %v", result.Logs)
}
