package operator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	item := Item{
		Type:     "osd",
		ID:       "123",
		FullName: "osd.123",
		Value:    1500.5,
		OsdID:    "123",
	}

	tests := []struct {
		template string
		expected string
	}{
		{"ceph osd ok-to-stop osd.{id}", "ceph osd ok-to-stop osd.123"},
		{"WOULD STOP: {full_name}", "WOULD STOP: osd.123"},
		{"osd.{osd_id} has value {value}", "osd.123 has value 1500.50"},
	}

	for _, test := range tests {
		result := render(test.template, item)
		if result != test.expected {
			t.Errorf("render(%q) = %q, expected %q", test.template, result, test.expected)
		}
	}
}

func TestLoadRule(t *testing.T) {
	// Create a temporary rules directory
	tmpDir := t.TempDir()
	ruleContent := `name: test_rule
description: Test rule
enabled: true
conditions:
  - command: ceph_companion.prometheus_fetcher.get_osd_latency
    threshold: 1000
action:
  type: ceph_cli
  command: "cephadm unit stop --name osd.{id}"
  max_items_per_run: 1
`
	rulePath := filepath.Join(tmpDir, "test_rule.yaml")
	if err := os.WriteFile(rulePath, []byte(ruleContent), 0644); err != nil {
		t.Fatalf("failed to write test rule: %v", err)
	}

	// Test loading rule
	eng := NewEngine(tmpDir, false, "", false)
	rule, err := eng.loadRule(rulePath)
	if err != nil {
		t.Fatalf("failed to load rule: %v", err)
	}

	if rule.Name != "test_rule" {
		t.Errorf("expected name 'test_rule', got '%s'", rule.Name)
	}
	if !rule.Enabled {
		t.Errorf("expected rule to be enabled")
	}
	if len(rule.Conditions) != 1 {
		t.Errorf("expected 1 condition, got %d", len(rule.Conditions))
	}
	if rule.Action.MaxItemsPerRun != 1 {
		t.Errorf("expected max_items_per_run 1, got %d", rule.Action.MaxItemsPerRun)
	}
}

func TestThresholdFilter(t *testing.T) {
	items := []Item{
		{ID: "1", Value: 100},
		{ID: "2", Value: 200},
		{ID: "3", Value: 50},
		{ID: "4", Value: 300},
	}

	filtered := thresholdFilter(items, 150, "")
	if len(filtered) != 2 {
		t.Errorf("expected 2 items above 150, got %d", len(filtered))
	}

	filtered = thresholdFilter(items, 150, "<")
	if len(filtered) != 2 {
		t.Errorf("expected 2 items below 150, got %d", len(filtered))
	}
}

func TestLoadRulesDeduplicatesByName(t *testing.T) {
	tmpDir := t.TempDir()
	ruleYAML := func(name string) string {
		return fmt.Sprintf(`name: %s
enabled: true
conditions:
  - command: prometheus_query
    query: "up"
    threshold: 0
action:
  type: ceph_cli
  command: "true"
`, name)
	}
	for _, f := range []string{"a_stop_osd.yaml", "b_stop_osd.yaml", "other.yaml"} {
		name := strings.TrimSuffix(f, ".yaml")
		if err := os.WriteFile(filepath.Join(tmpDir, f), []byte(ruleYAML(name)), 0644); err != nil {
			t.Fatal(err)
		}
	}

	eng := &Engine{RulesDir: tmpDir, Debug: true}
	rules, err := eng.loadRules("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("expected 3 unique rules, got %d", len(rules))
	}
	names := map[string]int{}
	for _, r := range rules {
		names[r.Name]++
	}
	for name, n := range names {
		if n != 1 {
			t.Errorf("rule %q loaded %d times, expected 1", name, n)
		}
	}
}

func TestOSDFilterMatcherEscapesDotForPromQL(t *testing.T) {
	kwargs := map[string]interface{}{
		"input_items": []Item{{Type: "osd", ID: "205"}},
	}
	got := osdFilterMatcher(kwargs)
	want := `ceph_daemon=~"osd\\.(205)"`
	if got != want {
		t.Errorf("osdFilterMatcher = %q, want %q (PromQL string escapes need a doubled backslash before the dot)", got, want)
	}
	// A single backslash is an invalid PromQL escape and makes the query
	// fail with HTTP 400.
	if strings.Count(got, `\.`) != 1 || strings.Count(got, `\\.`) != 1 {
		t.Errorf("expected exactly one doubled escape, got %q", got)
	}
}
