package ceph

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// installCephadmStub puts a fake `cephadm` first on PATH for the duration of
// the test. It records the arguments it was called with in argsFile and
// prints stdoutJSON on stdout (stderr message when failing is supported via
// exitCode/stderr).
func installCephadmStub(t *testing.T, argsFile, stdoutJSON string, exitCode int, stderr string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "cephadm")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\n", argsFile)
	if exitCode != 0 {
		script += fmt.Sprintf("echo %s >&2\nexit %d\n", quoteSh(stderr), exitCode)
	} else {
		script += fmt.Sprintf("echo %s\n", quoteSh(stdoutJSON))
	}
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readStubArgs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub did not run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func TestOSDPerfDumpRunsThroughCephadmShell(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installCephadmStub(t, argsFile, `{"ops": []}`, 0, "")

	c := NewClient()
	data, err := c.OSDPerfDump(205)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if data == nil {
		t.Fatal("expected parsed data, got nil")
	}

	want := []string{"shell", "ceph", "tell", "osd.205", "perf", "dump", "--format", "json"}
	if got := readStubArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("cephadm args = %v, want %v", got, want)
	}
}

func TestOSDHistoricOpsRunsThroughCephadmShell(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installCephadmStub(t, argsFile, `{"ops": []}`, 0, "")

	if _, err := NewClient().OSDHistoricOps(7); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	want := []string{"shell", "ceph", "tell", "osd.7", "dump_historic_ops", "--format", "json"}
	if got := readStubArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("cephadm args = %v, want %v", got, want)
	}
}

func TestOSDLsRunsThroughCephadmShell(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installCephadmStub(t, argsFile, `[1,2,3]`, 0, "")

	ids, err := NewClient().OSDLs()
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(ids) != 3 || ids[0] != 1 {
		t.Errorf("expected [1 2 3], got %v", ids)
	}

	want := []string{"shell", "ceph", "osd", "ls", "--format", "json"}
	if got := readStubArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("cephadm args = %v, want %v", got, want)
	}
}

func TestOSDPerfDumpSurfacesStderr(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installCephadmStub(t, argsFile, "", 1, "RADOS permission denied (error connecting to the cluster)")

	_, err := NewClient().OSDPerfDump(205)
	if err == nil {
		t.Fatal("expected an error when cephadm shell fails")
	}
	if !strings.Contains(err.Error(), "RADOS permission denied") {
		t.Errorf("expected stderr content in the error, got: %v", err)
	}
}

func TestOSDCounterDumpRunsThroughCephadmShell(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installCephadmStub(t, argsFile, `{}`, 0, "")

	if _, err := NewClient().OSDCounterDump(9); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	want := []string{"shell", "ceph", "tell", "osd.9", "counter", "dump", "--format", "json"}
	if got := readStubArgs(t, argsFile); !reflect.DeepEqual(got, want) {
		t.Errorf("cephadm args = %v, want %v", got, want)
	}
}

func TestRunRejectsBadJSON(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	installCephadmStub(t, argsFile, "not json at all", 0, "")

	c := NewClient()
	if _, err := c.Run([]string{"true"}); err == nil {
		t.Error("expected JSON parse error for non-JSON stdout")
	}
}
