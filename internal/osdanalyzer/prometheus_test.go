package osdanalyzer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// captureStdout redirects the process stdout to a pipe for the duration of fn
// and returns everything written (the analyzer prints via fmt.Print*).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// promStub is a fake Prometheus /api/v1/query endpoint. queryLog records every
// query string received, in order; queries are answered by responder.
type promStub struct {
	server    *httptest.Server
	queryLog  []string
	responder func(query string) (results []prometheus.PrometheusResult)
}

func newPromStub(t *testing.T, responder func(query string) []prometheus.PrometheusResult) *promStub {
	t.Helper()
	ps := &promStub{responder: responder}
	ps.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		ps.queryLog = append(ps.queryLog, q)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"resultType": "vector",
				"result":     responder(q),
			},
		})
	}))
	t.Cleanup(ps.server.Close)
	return ps
}

func (ps *promStub) cfg() *config.PrometheusConfig {
	return &config.PrometheusConfig{
		URL:       ps.server.URL,
		ClusterID: "cid-123",
	}
}

func newWiredAnalyzer(t *testing.T, ps *promStub) *Analyzer {
	t.Helper()
	a := NewAnalyzer()
	a.SetPrometheus(ps.cfg(), prometheus.NewClient())
	return a
}

func TestPromQueryForTokenRawMetric(t *testing.T) {
	expr, ok := promQueryForToken("ceph_bluestore_slow_committed_kv_count")
	if !ok {
		t.Fatal("expected raw metric token to map")
	}
	if expr != "ceph_bluestore_slow_committed_kv_count{SEL}" {
		t.Errorf("unexpected expr: %q", expr)
	}
}

func TestPromQueryForTokenPerfPaths(t *testing.T) {
	cases := []struct{ token, want string }{
		{"bluestore.state_done_lat.avgtime", "ceph_bluestore_state_done_lat_sum{SEL} / ceph_bluestore_state_done_lat_count{SEL}"},
		{"osd.op_r_latency.avgtime", "ceph_osd_op_r_latency_sum{SEL} / ceph_osd_op_r_latency_count{SEL}"},
		{"osd-slow-ops.slow_ops_count", "ceph_osd_slow_ops_slow_ops_count{SEL}"},
		{"bluestore.txc_commit_lat.avgcount", "ceph_bluestore_txc_commit_lat_count{SEL}"},
		{"bluestore.txc_commit_lat.sum", "ceph_bluestore_txc_commit_lat_sum{SEL}"},
		{"bluestore.txc_commit_lat.value", "ceph_bluestore_txc_commit_lat{SEL}"},
		{"nope", ""},
		{"a.b.c.d", ""},
	}
	for _, c := range cases {
		expr, ok := promQueryForToken(c.token)
		if c.want == "" {
			if ok {
				t.Errorf("token %q: expected no mapping, got %q", c.token, expr)
			}
			continue
		}
		if !ok || expr != c.want {
			t.Errorf("token %q: got %q (ok=%v), want %q", c.token, expr, ok, c.want)
		}
	}
}

func TestPromResolverRawMetric(t *testing.T) {
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult {
		if !strings.Contains(q, `ceph_bluestore_slow_committed_kv_count{ceph_daemon='osd.205',cluster='cid-123'}`) {
			t.Errorf("unexpected query: %q", q)
		}
		return []prometheus.PrometheusResult{
			{Metric: map[string]string{"ceph_daemon": "osd.205"}, Value: []interface{}{0.0, "7"}},
		}
	})
	a := newWiredAnalyzer(t, ps)

	result := EvaluatePatternWithResolver("ceph_bluestore_slow_committed_kv_count > 0", nil, "prometheus", a.promResolver(205))
	if !result.Success {
		t.Errorf("expected 7 > 0 to trigger, message: %s", result.Message)
	}
	if v, ok := result.FoundValues["ceph_bluestore_slow_committed_kv_count"].(float64); !ok || v != 7 {
		t.Errorf("expected resolved value 7, got %v", result.FoundValues["ceph_bluestore_slow_committed_kv_count"])
	}
}

func TestPromResolverAvgTimeMapping(t *testing.T) {
	// The avgtime token becomes a single division query; a real Prometheus
	// evaluates it server-side and returns the quotient as one series.
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult {
		if !strings.Contains(q, "state_done_lat_sum") || !strings.Contains(q, "state_done_lat_count") {
			t.Errorf("unexpected query: %q", q)
		}
		return []prometheus.PrometheusResult{{Metric: map[string]string{"ceph_daemon": "osd.205"}, Value: []interface{}{0.0, "5"}}}
	})
	a := newWiredAnalyzer(t, ps)

	result := EvaluatePatternWithResolver("bluestore.state_done_lat.avgtime > 1", nil, "prometheus", a.promResolver(205))
	if !result.Success {
		t.Errorf("expected 5.0 > 1 to trigger, message: %s (values: %v)", result.Message, result.FoundValues)
	}
	if v, ok := result.FoundValues["bluestore.state_done_lat.avgtime"].(float64); !ok || v != 5 {
		t.Errorf("expected resolved value 5, got %v", result.FoundValues["bluestore.state_done_lat.avgtime"])
	}
}

func TestPromResolverFallsBackToOsdLabel(t *testing.T) {
	var queries atomic.Int32
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult {
		queries.Add(1)
		if strings.Contains(q, `osd='205'`) {
			return []prometheus.PrometheusResult{{Metric: map[string]string{"osd": "205"}, Value: []interface{}{0.0, "3"}}}
		}
		return nil // ceph_daemon selector matches nothing on this exporter
	})
	a := newWiredAnalyzer(t, ps)

	result := EvaluatePatternWithResolver("ceph_weird_metric > 2", nil, "prometheus", a.promResolver(205))
	if !result.Success {
		t.Errorf("expected osd-label fallback to resolve value 3, message: %s", result.Message)
	}
}

func TestPromResolverMissingSeriesSkipsEvaluation(t *testing.T) {
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult { return nil })
	a := newWiredAnalyzer(t, ps)

	result := EvaluatePatternWithResolver("ceph_no_such_metric > 0", nil, "prometheus", a.promResolver(205))
	if result.Success {
		t.Error("expected evaluation to be skipped when the metric has no series")
	}
	if result.FoundValues["ceph_no_such_metric"] != "no data found" {
		t.Errorf("expected 'no data found', got %v", result.FoundValues["ceph_no_such_metric"])
	}
}

func TestPromResolverCachesPerQuery(t *testing.T) {
	var calls atomic.Int32
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult {
		calls.Add(1)
		return []prometheus.PrometheusResult{{Metric: map[string]string{}, Value: []interface{}{0.0, "1"}}}
	})
	a := newWiredAnalyzer(t, ps)
	r := a.promResolver(205)

	r("ceph_metric_one")
	r("ceph_metric_one")
	r("ceph_metric_two")
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 HTTP queries (one per metric, cached per query), got %d", got)
	}
}

func TestPrometheusSourceRejectsCephJSONFunctions(t *testing.T) {
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult { return nil })
	a := newWiredAnalyzer(t, ps)

	for _, pattern := range []string{
		"TOP(5, ops)",
		"count(events) > 5",
		"event_duration('osd_op') > 1",
		"list_events()",
		"list_clients()",
		"scrub_blocked_by(1) > 0",
	} {
		result := EvaluatePatternWithResolver(pattern, nil, "prometheus", a.promResolver(205))
		if result.Success {
			t.Errorf("pattern %q should not succeed under source prometheus", pattern)
		}
		if !strings.Contains(result.Message, "not supported") {
			t.Errorf("pattern %q: expected a clear 'not supported' message, got: %s", pattern, result.Message)
		}
	}
}

func TestPrometheusWithoutClusterReportsConfigError(t *testing.T) {
	// A bare Analyzer (no SetCluster) must not silently mis-evaluate.
	a := NewAnalyzer()
	pattern := &AnalyzerPattern{Name: "x", Source: "prometheus", Patterns: []string{"ceph_bluestore_slow_committed_kv_count > 0"}}
	captureStdout(t, func() {
		results := a.RunPattern(pattern, 205, nil, "", true)
		if results != nil {
			t.Errorf("expected no results without a configured cluster, got %v", results)
		}
	})
}

func TestRunPatternSurfacesFetchError(t *testing.T) {
	// No cephadm stub on PATH here, so exec fails -> the analyzer must
	// report the real error instead of the old "No data available" line.
	a := NewAnalyzer()
	pattern := &AnalyzerPattern{Name: "x", Source: "osd_perf_dump", Patterns: []string{"bluestore.slow_committed_kv_count > 0"}}

	out := captureStdout(t, func() {
		a.RunPattern(pattern, 205, nil, "", true)
	})
	if !strings.Contains(out, "Failed to fetch osd_perf_dump for OSD 205") {
		t.Errorf("expected the fetch error to be surfaced, got: %s", out)
	}
	if strings.Contains(out, "No data available from source") {
		t.Errorf("expected the misleading 'No data available' line to be replaced by the error, got: %s", out)
	}
}

func TestFetchErrorPrintedOncePerSourceAndOSD(t *testing.T) {
	a := NewAnalyzer()
	pattern := &AnalyzerPattern{Name: "x", Source: "osd_perf_dump", Patterns: []string{"bluestore.slow_committed_kv_count > 0"}}

	out := captureStdout(t, func() {
		a.RunPattern(pattern, 205, nil, "", true)
		a.RunPattern(pattern, 205, nil, "", true)
	})
	full := strings.Count(out, "Failed to fetch osd_perf_dump for OSD 205")
	cached := strings.Count(out, "(fetch failed, see error above)")
	if full != 1 || cached != 1 {
		t.Errorf("expected 1 full error + 1 cached-error note, got full=%d cached=%d in:\n%s", full, cached, out)
	}
}

func TestFetchCacheAvoidsRefetch(t *testing.T) {
	// Stub `cephadm` with a script that appends one line per invocation, then
	// run the same pattern file twice: the second run must reuse the cached
	// osd_perf_dump fetch instead of shelling out again.
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	stub := filepath.Join(dir, "cephadm")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'x' >> %s\necho '{}'\n", counter)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := NewAnalyzer()
	pattern := &AnalyzerPattern{Name: "x", Source: "osd_perf_dump", Patterns: []string{"bluestore.slow_committed_kv_count > 0"}}
	captureStdout(t, func() {
		a.RunPattern(pattern, 205, nil, "", true)
		a.RunPattern(pattern, 205, nil, "", true)
	})

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("stub never ran: %v", err)
	}
	if got := string(raw); got != "x" {
		t.Errorf("expected exactly 1 fetch for 2 RunPattern calls, got %d", len(got))
	}
}

func TestUnknownSourceIsReported(t *testing.T) {
	a := NewAnalyzer()
	pattern := &AnalyzerPattern{Name: "x", Source: "bogus_source", Patterns: []string{"foo.bar > 1"}}
	out := captureStdout(t, func() {
		a.RunPattern(pattern, 205, nil, "", true)
	})
	if !strings.Contains(out, `unknown data source "bogus_source"`) {
		t.Errorf("expected unknown-source error, got: %s", out)
	}
}

func TestPromQueryForTokenAndCacheAcrossPatternFiles(t *testing.T) {
	var calls atomic.Int32
	ps := newPromStub(t, func(q string) []prometheus.PrometheusResult {
		calls.Add(1)
		return []prometheus.PrometheusResult{{Metric: map[string]string{}, Value: []interface{}{0.0, "1"}}}
	})
	a := newWiredAnalyzer(t, ps)
	pattern := &AnalyzerPattern{Name: "x", Source: "prometheus", Patterns: []string{"ceph_bluestore_slow_aio_wait_count > 0"}}

	captureStdout(t, func() {
		a.RunPattern(pattern, 205, nil, "", true)
		a.RunPattern(pattern, 205, nil, "", true)
	})
	if got := calls.Load(); got != 1 {
		t.Errorf("expected the prometheus value to be cached across RunPattern calls, got %d queries", got)
	}
}
