package operator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/ceph"
)

// osdNames prefixes plain OSD IDs with "osd." - the form cephadm receives
// in `unit status --name osd.X`.
func osdNames(ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, fmt.Sprintf("osd.%s", id))
	}
	return strings.Join(names, "|")
}

// installOkToStopStubs puts a cephadm stub on PATH whose answers are keyed
// by OSD ID: `cephadm unit status --name osd.X` exits 1 (stopped) for the
// stopped IDs, and `ceph osd ok-to-stop <id>` (bare ID, as OSDOkToStop
// sends it - it's the command's last argument) exits 1 (not safe) for the
// unsafe IDs. Everything else reports running and safe.
func installOkToStopStubs(t *testing.T, stopped, unsafe []string) {
	t.Helper()
	dir := t.TempDir()

	stoppedCheck, unsafeCheck := "if false; then exit 1; fi", "if false; then exit 1; fi"
	if len(stopped) > 0 {
		stoppedCheck = fmt.Sprintf(`case "$name" in %s) exit 1 ;; esac`, osdNames(stopped))
	}
	if len(unsafe) > 0 {
		unsafeCheck = fmt.Sprintf(`case "$last" in %s) exit 1 ;; esac`, strings.Join(unsafe, "|"))
	}
	cephadm := fmt.Sprintf(`#!/bin/sh
name=""
last=""
for arg in "$@"; do
  [ "$prev" = "--name" ] && name="$arg"
  last="$arg"
  prev="$arg"
done
if [ "$1" = "unit" ]; then
  %s
  exit 0
fi
if [ "$1" = "shell" ]; then
  %s
  exit 0
fi
exit 0
`, stoppedCheck, unsafeCheck)
	if err := os.WriteFile(filepath.Join(dir, "cephadm"), []byte(cephadm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestEvalOSDOkToStopStoppedOSDsDoNotBlock replays the incident that
// stranded the last OSD: an NVMe accelerator hosts several OSDs, earlier
// runs (max_items_per_run: 1) already stopped three of them, and the
// "all safe" veto from the already-stopped peers kept the last one from
// ever being stopped (cephosd-1, 2026-10-06). Already-stopped OSDs
// are the desired protective state - they must not block the rest.
func TestEvalOSDOkToStopStoppedOSDsDoNotBlock(t *testing.T) {
	installOkToStopStubs(t, []string{"44", "46", "52"}, nil)

	eng := &Engine{CephClient: ceph.NewClient()}
	items := []Item{
		{Type: "osd", ID: "44"}, {Type: "osd", ID: "45"},
		{Type: "osd", ID: "46"}, {Type: "osd", ID: "52"},
	}
	result := eng.evalOSDOkToStop(map[string]interface{}{"input_items": items}, &RuleCondition{})

	if !result.Triggered {
		t.Fatalf("expected trigger for the one still-running safe OSD, logs: %v, errors: %v", result.Logs, result.Errors)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "45" || !result.Items[0].SafeToStop {
		t.Errorf("expected exactly osd.45 as the actionable item, got %v", result.Items)
	}
	joined := strings.Join(result.Logs, "\n")
	for _, want := range []string{
		"osd.44: already stopped", "osd.46: already stopped",
		"osd.52: already stopped", "osd.45: safe",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("logs missing %q, got: %s", want, joined)
		}
	}
}

func TestEvalOSDOkToStopAllStoppedNoTrigger(t *testing.T) {
	installOkToStopStubs(t, []string{"44", "45"}, nil)

	eng := &Engine{CephClient: ceph.NewClient()}
	items := []Item{{Type: "osd", ID: "44"}, {Type: "osd", ID: "45"}}
	result := eng.evalOSDOkToStop(map[string]interface{}{"input_items": items}, &RuleCondition{})

	if result.Triggered {
		t.Errorf("expected no trigger when every OSD is already stopped, items: %v", result.Items)
	}
	if len(result.Items) != 0 {
		t.Errorf("expected no actionable items, got %v", result.Items)
	}
	if len(result.Errors) != 0 {
		t.Errorf("expected no errors, got %v", result.Errors)
	}
}

// A still-running OSD that ok-to-stop rejects must keep vetoing the
// condition - only already-stopped ones are neutralized.
func TestEvalOSDOkToStopUnsafeStillBlocks(t *testing.T) {
	installOkToStopStubs(t, []string{"44"}, []string{"45"})

	eng := &Engine{CephClient: ceph.NewClient()}
	items := []Item{{Type: "osd", ID: "44"}, {Type: "osd", ID: "45"}}
	result := eng.evalOSDOkToStop(map[string]interface{}{"input_items": items}, &RuleCondition{})

	if result.Triggered {
		t.Errorf("expected the unsafe OSD to veto the condition, items: %v", result.Items)
	}
	joined := strings.Join(result.Logs, "\n")
	if !strings.Contains(joined, "osd.44: already stopped") || !strings.Contains(joined, "osd.45: NOT safe") {
		t.Errorf("expected both the stopped and NOT-safe lines in logs, got: %s", joined)
	}
}

func TestEvalOSDOkToStopAllSafeTriggers(t *testing.T) {
	installOkToStopStubs(t, nil, nil)

	eng := &Engine{CephClient: ceph.NewClient()}
	items := []Item{{Type: "osd", ID: "44"}, {Type: "osd", ID: "45"}}
	result := eng.evalOSDOkToStop(map[string]interface{}{"input_items": items}, &RuleCondition{})

	if !result.Triggered {
		t.Fatalf("expected trigger when every OSD is running and safe, logs: %v", result.Logs)
	}
	if len(result.Items) != 2 {
		t.Errorf("expected both OSDs as actionable items, got %v", result.Items)
	}
	for _, it := range result.Items {
		if !it.SafeToStop {
			t.Errorf("expected %s to be safe to stop, got %v", it.FullName, it)
		}
	}
}
