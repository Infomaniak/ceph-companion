package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Test with missing file
	_, err := LoadConfig("nonexistent.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}

	// Test with valid config
	tmpDir := t.TempDir()
	configContent := `prometheus:
  url: "http://localhost:9090"
  cluster_id: "test-id"
  username: "monitoring"
  password: "secret"
`
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Prometheus == nil {
		t.Fatal("expected prometheus section to be set")
	}
	if cfg.Prometheus.URL != "http://localhost:9090" {
		t.Errorf("unexpected prometheus URL: %s", cfg.Prometheus.URL)
	}
	if cfg.Prometheus.ClusterID != "test-id" {
		t.Errorf("unexpected cluster ID: %s", cfg.Prometheus.ClusterID)
	}
	if cfg.Prometheus.Username != "monitoring" || cfg.Prometheus.Password != "secret" {
		t.Errorf("unexpected basic-auth credentials: %s/%s", cfg.Prometheus.Username, cfg.Prometheus.Password)
	}
}

func TestLoadConfigRejectsOldFormat(t *testing.T) {
	cases := []string{
		"clusters:\n  - name: test\n    prometheus_url: \"http://localhost:9090\"\n",
		"default_cluster: test\n",
	}
	for i, content := range cases {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.yaml")
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(configPath)
		if err == nil {
			t.Fatalf("case %d: expected old-format config to be rejected", i)
		}
		if !strings.Contains(err.Error(), "config format changed") {
			t.Errorf("case %d: expected a format-change error, got: %v", i, err)
		}
	}
}

func TestPrometheus(t *testing.T) {
	if _, err := Prometheus(nil); err == nil {
		t.Error("expected error for nil config")
	}

	p, err := Prometheus(&Config{})
	if err == nil {
		t.Error("expected error when prometheus section is missing")
	}
	if p != nil {
		t.Errorf("expected nil config on error, got %+v", p)
	}

	if _, err := Prometheus(&Config{Prometheus: &PrometheusConfig{}}); err == nil {
		t.Error("expected error when prometheus.url is empty")
	}
	if _, err := Prometheus(&Config{Prometheus: &PrometheusConfig{URL: "http://localhost:9090"}}); err == nil {
		t.Error("expected error when prometheus.cluster_id is empty")
	}

	want := &PrometheusConfig{URL: "http://localhost:9090", ClusterID: "test-id"}
	got, err := Prometheus(&Config{Prometheus: want})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("expected the configured section to be returned, got %+v", got)
	}
}

func TestPrometheusConfigLabelName(t *testing.T) {
	p := PrometheusConfig{}
	if p.LabelName() != "cluster" {
		t.Errorf("expected default label 'cluster', got %q", p.LabelName())
	}
	p.ClusterLabel = "ceph_cluster"
	if p.LabelName() != "ceph_cluster" {
		t.Errorf("expected configured label, got %q", p.LabelName())
	}
}

func TestPrometheusConfigQueryURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:9090":                 "http://localhost:9090/api/v1/query",
		"http://localhost:9090/":                "http://localhost:9090/api/v1/query",
		"https://prom.example.com":              "https://prom.example.com/api/v1/query",
		"https://prom.example.com/sub":          "https://prom.example.com/sub/api/v1/query",
		"https://host.example.com/api/v1/query": "https://host.example.com/api/v1/query",
	}
	for in, want := range cases {
		if got := (PrometheusConfig{URL: in}).QueryURL(); got != want {
			t.Errorf("QueryURL(%q) = %q, want %q", in, got, want)
		}
	}
}
