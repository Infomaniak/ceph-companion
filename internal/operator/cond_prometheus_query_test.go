package operator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// promQueryStub answers /api/v1/query with the JSON payload the test
// registers per query substring.
func promQueryStub(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		for substr, body := range responses {
			if strings.Contains(query, substr) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
				return
			}
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func prometheusConfig(url string) *config.Config {
	return &config.Config{
		Prometheus: &config.PrometheusConfig{URL: url, ClusterID: "test-cluster-id"},
	}
}

// TestEvalPrometheusQueryEmptyResultIsLegitimate pins the wording for the
// empty-vector case: rules with an embedded filter like
// topk(5, (increase(...) > 0)) legitimately return zero series when every
// OSD is clean, and that must not read as missing metric data.
func TestEvalPrometheusQueryEmptyResultIsLegitimate(t *testing.T) {
	srv := promQueryStub(t, map[string]string{
		"*": `{"status":"success","data":{"resultType":"vector","result":[]}}`,
	})
	eng := &Engine{PromClient: prometheus.NewClient(), Config: prometheusConfig(srv.URL)}

	threshold := 2.0
	cond := &RuleCondition{
		Query:      "topk(5, (increase(ceph_bluestore_slow_committed_kv_count{cluster_selector}[1h]) > 0))",
		Operator:   ">",
		Threshold:  &threshold,
		EntityType: "osd",
	}
	result := eng.evalPrometheusQuery(map[string]interface{}{}, cond)

	if result.Triggered {
		t.Errorf("expected no trigger on empty result, logs: %v", result.Logs)
	}
	joined := strings.Join(result.Logs, "\n")
	if !strings.Contains(joined, "query returned no series") {
		t.Errorf("expected the empty-result explanation, got: %s", joined)
	}
	if !strings.Contains(joined, `embedded "> 0"`) {
		t.Errorf("expected the message to point at the query's own filter, got: %s", joined)
	}
}

func TestEvalPrometheusQueryTriggersOnSeries(t *testing.T) {
	srv := promQueryStub(t, map[string]string{
		"ceph_bluestore": `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"osd":"osd.51"},"value":[1725868800,"3"]}
		]}}`,
	})
	eng := &Engine{PromClient: prometheus.NewClient(), Config: prometheusConfig(srv.URL)}

	threshold := 2.0
	cond := &RuleCondition{
		Query:      "topk(5, increase(ceph_bluestore_slow_committed_kv_count{cluster_selector}[1h]))",
		Operator:   ">",
		Threshold:  &threshold,
		EntityType: "osd",
	}
	result := eng.evalPrometheusQuery(map[string]interface{}{}, cond)

	if !result.Triggered {
		t.Errorf("expected trigger with value 3 > threshold 2, logs: %v", result.Logs)
	}
	if len(result.Items) != 1 || result.Items[0].FullName != "osd.51" {
		t.Errorf("expected item osd.51, got %v", result.Items)
	}
}
