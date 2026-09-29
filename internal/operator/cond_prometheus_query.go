package operator

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// Command: prometheus_query - the generic condition. New metric-based
// checks are added by writing a rule file, not Go code; see
// rules/example_generic_query.yaml.

// defaultEntityLabels are the Prometheus label names tried, in order, to
// name a result when a condition doesn't specify its own entity_labels.
var defaultEntityLabels = []string{"ceph_daemon", "osd", "instance"}

// osdFilterMatcher builds a Prometheus regex label matcher (e.g.
// `ceph_daemon=~"osd\\.(5|12)"`) narrowing a query to the OSD IDs an earlier
// chained condition already flagged, or "" if there's nothing to narrow by.
// Without this, chained per-OSD conditions each re-query independently and
// "AND" across them only means "all triggered," not "same OSD."
//
// Note the doubled backslash: PromQL unescapes the double-quoted string, so
// the regex engine must receive `osd\.` - a single backslash before the dot
// is an invalid string escape ("unknown escape sequence U+002E").
func osdFilterMatcher(kwargs map[string]interface{}) string {
	inputItems, ok := kwargs["input_items"].([]Item)
	if !ok {
		return ""
	}
	var ids []string
	for _, it := range inputItems {
		if it.Type == "osd" && it.ID != "" {
			ids = append(ids, regexp.QuoteMeta(it.ID))
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf(`ceph_daemon=~"osd\\.(%s)"`, strings.Join(ids, "|"))
}

// clusterSelector builds the "{cluster_selector}" replacement for
// prometheus_query: the cluster's ID matcher combined with OSD narrowing
// from osdFilterMatcher, in one brace group (PromQL doesn't allow two
// separate {...} groups on the same metric).
func clusterSelector(labelName, clusterID string, kwargs map[string]interface{}) string {
	var parts []string
	if clusterID != "" {
		parts = append(parts, fmt.Sprintf("%s='%s'", labelName, clusterID))
	}
	if m := osdFilterMatcher(kwargs); m != "" {
		parts = append(parts, m)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// evalPrometheusQuery is a generic condition: it runs the PromQL given in the
// rule YAML (`query:`) and compares each result against `threshold`/
// `operator`. This lets a new metric-based condition be added by editing a
// rule file only - no new Go eval function or engine.go registration needed.
//
// The query string may reference "{cluster_selector}" (replaced with a label
// matcher such as "{cluster='abc'}" scoping the query to the configured
// cluster, automatically narrowed to the OSDs found by an earlier chained
// condition, or "" if neither applies) and "{cluster_id}" (replaced with the
// bare ID).
func (e *Engine) evalPrometheusQuery(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	if cond.Query == "" {
		return &EvalResult{Triggered: false, Logs: []string{"prometheus_query condition requires a 'query'"}}
	}

	p, err := e.promConfig()
	if err != nil {
		return &EvalResult{Triggered: false, Errors: []string{err.Error()}}
	}

	entityType := cond.EntityType
	if entityType == "" {
		entityType = "item"
	}
	entityLabels := cond.EntityLabels
	if len(entityLabels) == 0 {
		entityLabels = defaultEntityLabels
	}
	threshold := 0.0
	if cond.Threshold != nil {
		threshold = *cond.Threshold
	}

	var allItems []Item
	var logs []string
	triggered := false

	auth := prometheus.NewAuth(p.Username, p.Password)

	selector := clusterSelector(p.LabelName(), p.ClusterID, kwargs)
	query := strings.ReplaceAll(cond.Query, "{cluster_selector}", selector)
	query = strings.ReplaceAll(query, "{cluster_id}", p.ClusterID)

	// The engine prints condition logs only in debug mode, so logging
	// the resolved query unconditionally is safe - and it's the first
	// thing you need when values look wrong.
	logs = append(logs, fmt.Sprintf("  query: %s", query))

	results, err := e.PromClient.Query(p.QueryURL(), query, auth)
	if err != nil {
		return &EvalResult{Items: allItems, Triggered: triggered, Errors: []string{fmt.Sprintf("prometheus query failed: %v", err)}}
	}

	if len(results) == 0 {
		// An empty vector is usually legitimate, not a data problem:
		// queries like topk(5, (increase(...) > 0)) filter out every
		// series themselves when nothing is above 0, which is exactly
		// the healthy case. Say so instead of implying the cluster's
		// data is missing.
		logs = append(logs, "query returned no series - usually just the query's own filter (e.g. an embedded \"> 0\") matching nothing, not missing data")
		return &EvalResult{Items: allItems, Triggered: triggered, Logs: logs}
	}

	for _, item := range results {
		name := prometheus.FirstLabel(item.Metric, entityLabels)
		if name == "" {
			name = "unknown"
		}
		value := prometheus.ExtractValue(item)
		logs = append(logs, fmt.Sprintf("  %s %s: %.2f", entityType, name, value))

		if compareValue(value, cond.Operator, threshold) {
			triggered = true
		}

		allItems = append(allItems, Item{
			Type:     entityType,
			ID:       strings.TrimPrefix(name, entityType+"."),
			FullName: name,
			Value:    value,
		})
	}

	return &EvalResult{Items: allItems, Triggered: triggered, Logs: logs}
}
