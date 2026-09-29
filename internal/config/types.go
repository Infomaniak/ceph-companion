package config

import (
	"strings"
)

// PrometheusConfig is the `prometheus:` section of config.yaml: the single
// endpoint the companion queries, plus optional query-scoping and
// basic-auth settings. The endpoint may host metrics for several Ceph
// clusters - queries are scoped to the configured one via ClusterID.
type PrometheusConfig struct {
	URL string `yaml:"url"`
	// ClusterID is the value carried by the cluster label on ceph exporter
	// metrics (e.g. `cluster='<fsid>'`). It is mandatory: every query is
	// scoped to it (the endpoint may host metrics for several Ceph
	// clusters), and rule queries can reference it via the {cluster_id}
	// placeholder.
	ClusterID string `yaml:"cluster_id"`
	// ClusterLabel is the name of the label carrying ClusterID (e.g.
	// "cluster", "ceph_cluster", "job"). Defaults to "cluster" when empty -
	// see LabelName().
	ClusterLabel string `yaml:"cluster_label,omitempty"`
	// Username/Password are optional basic-auth credentials for endpoints
	// secured behind a reverse proxy. Basic auth is applied whenever
	// Username is non-empty (see prometheus.NewAuth).
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// LabelName returns the Prometheus label used to identify the cluster,
// falling back to "cluster" when not explicitly configured.
func (p PrometheusConfig) LabelName() string {
	if p.ClusterLabel != "" {
		return p.ClusterLabel
	}
	return "cluster"
}

// QueryURL returns the instant-query endpoint for this Prometheus. Any
// subpath in the URL (e.g. a VictoriaMetrics endpoint served under
// "/prometheus") is preserved; a URL already ending in the query path is
// returned unchanged.
func (p PrometheusConfig) QueryURL() string {
	base := strings.TrimSuffix(p.URL, "/")
	if strings.HasSuffix(base, "/api/v1/query") {
		return base
	}
	return base + "/api/v1/query"
}

// Config represents the full configuration file
type Config struct {
	// Prometheus is the single configured endpoint. Commands and rule
	// conditions that need it fail with an actionable error when the
	// section is missing or has no URL (see config.Prometheus).
	Prometheus    *PrometheusConfig   `yaml:"prometheus,omitempty"`
	Notifications *NotificationConfig `yaml:"notifications,omitempty"`
}

// NotificationConfig represents webhook notification settings (e.g. kChat).
type NotificationConfig struct {
	Channel  string `yaml:"channel"`
	Username string `yaml:"username,omitempty"`
	Emoji    string `yaml:"emoji,omitempty"`
	Webhook  string `yaml:"webhook"`
	// ErrorChannel/ErrorWebhook optionally route execution-error
	// notifications (failed condition evaluations, failed actions) to a
	// dedicated spam-tolerant channel. When unset, errors go to the regular
	// Channel/Webhook above.
	ErrorChannel string `yaml:"error_channel,omitempty"`
	ErrorWebhook string `yaml:"error_webhook,omitempty"`
}
