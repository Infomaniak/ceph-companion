package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// SystemDir is the base directory searched for config.yaml, rules/, and
// patterns/ as a last resort, so an install with the binary in e.g.
// /usr/local/bin still finds its data files regardless of the working
// directory it's run from.
const SystemDir = "/etc/ceph-companion"

// LoadConfig loads configuration from the given YAML file. The path is used
// strictly: when the file can't be read, the error names it and points at
// the ways to override it (no ./config.yaml auto-detection).
func LoadConfig(configFile string) (*Config, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("config file not found: %s (use --config or $CEPH_COMPANION_CONFIG)", configFile)
	}

	// The pre-single-endpoint config format was a `clusters:` list selected
	// with default_cluster / --cluster; both are gone. Fail loudly instead
	// of silently running with no Prometheus endpoint while a stale
	// deployed config is still in place.
	var probe struct {
		Clusters       yaml.Node `yaml:"clusters"`
		DefaultCluster yaml.Node `yaml:"default_cluster"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil && (!probe.Clusters.IsZero() || !probe.DefaultCluster.IsZero()) {
		return nil, fmt.Errorf("config format changed: the 'clusters:' list and 'default_cluster' were replaced by a single 'prometheus:' section with a 'url' key (see etc/ceph-companion/configs/config.yaml)")
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}

// Prometheus returns the configured prometheus section, verifying the
// mandatory fields are set. Everything that queries the endpoint goes
// through this instead of dereferencing cfg.Prometheus directly, so a
// missing section always produces the same actionable error.
func Prometheus(cfg *Config) (*PrometheusConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no config loaded")
	}
	p := cfg.Prometheus
	if p == nil {
		return nil, fmt.Errorf("no prometheus section configured in config.yaml")
	}
	if p.URL == "" {
		return nil, fmt.Errorf("prometheus.url is required in config.yaml")
	}
	if p.ClusterID == "" {
		return nil, fmt.Errorf("prometheus.cluster_id is required in config.yaml (the value of the cluster label on ceph metrics, e.g. the fsid)")
	}
	return p, nil
}
