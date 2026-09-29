package operator

// Item represents a generic entity for the rule engine
type Item struct {
	Type           string  `json:"type"`
	ID             string  `json:"id"`
	FullName       string  `json:"full_name"`
	Value          float64 `json:"value,omitempty"`
	SafeToStop     bool    `json:"safe_to_stop,omitempty"`
	OsdID          string  `json:"osd_id,omitempty"`
	KernelDevice   string  `json:"kernel_device,omitempty"`
	ResolvedDevice string  `json:"resolved_device,omitempty"`
	ErrorCount     int     `json:"error_count,omitempty"`
}

// RuleAction represents an action in a rule
type RuleAction struct {
	Type             string `yaml:"type"`
	Command          string `yaml:"command"`
	DryRunText       string `yaml:"dry_run_text,omitempty"`
	MaxItemsPerRun   int    `yaml:"max_items_per_run,omitempty"`
	SendNotification bool   `yaml:"send-notif,omitempty"`
	NotificationMsg  string `yaml:"notif-message,omitempty"`
}

// RuleCondition represents a condition in a rule
type RuleCondition struct {
	Command   string   `yaml:"command"`
	Threshold *float64 `yaml:"threshold,omitempty"`
	// Operator is the comparison used against Threshold: >, >=, <, <=, ==, !=
	// (defaults to ">"). Applies to every condition type, including the
	// engine's post-condition item filtering.
	Operator string `yaml:"operator,omitempty"`

	// The fields below are only used by the generic "prometheus_query"
	// command, which lets a rule define a new metric-based condition
	// entirely in YAML instead of requiring a new Go eval function.
	//
	// Query is a PromQL string. It may reference "{cluster_selector}"
	// (replaced with a label matcher like "{cluster='id'}", or "" if the
	// cluster has no resolvable ID) and "{cluster_id}" (replaced with the
	// bare cluster ID).
	Query string `yaml:"query,omitempty"`
	// EntityType tags produced items (e.g. "osd", "pool", "rgw").
	EntityType string `yaml:"entity_type,omitempty"`
	// EntityLabels lists Prometheus label names to try, in order, to name
	// each result (defaults to ceph_daemon, osd, instance).
	EntityLabels []string `yaml:"entity_labels,omitempty"`

	// The fields below are only used by the "kernel_disk_errors" command.
	Minutes        *int `yaml:"minutes,omitempty"`         // lookback window (default: 60)
	ErrorLimit     *int `yaml:"error_limit,omitempty"`     // min error hits to flag a device (default: 1)
	RotationalOnly bool `yaml:"rotational_only,omitempty"` // skip SSD/NVMe, only consider HDDs
}

// Rule represents a single rule definition
type Rule struct {
	Name        string          `yaml:"name"`
	Description string          `yaml:"description,omitempty"`
	Enabled     bool            `yaml:"enabled"`
	Conditions  []RuleCondition `yaml:"conditions"`
	Action      RuleAction      `yaml:"action"`
}

// EvalResult represents evaluation result (items, triggered, logs)
type EvalResult struct {
	Items     []Item
	Triggered bool
	Logs      []string
	// Errors carries execution failures that left the condition unable to
	// produce a verdict (Prometheus query failures, missing metric data,
	// cephadm/journalctl exec failures, ...). Unlike Logs these are printed
	// on every run and batched into one kChat execution-error notification
	// per run (see Engine.notifyExecutionErrors) - a condition that cannot
	// evaluate must never be silently indistinguishable from a healthy one.
	Errors []string
	// AlwaysLog carries lines that must reach the output on every run,
	// even in live mode without --debug and even when the condition didn't
	// trigger - e.g. stale cache entries invalidated by reconciliation.
	// The engine prints these unconditionally (see Engine.evalRule), the
	// regular Logs only on trigger or in debug mode.
	AlwaysLog []string
}
