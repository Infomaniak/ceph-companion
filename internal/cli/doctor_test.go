package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

func TestCheckCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fakecmd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	results := checkCommands([]execBinary{
		{Name: "fakecmd", UsedBy: "test"},
		{Name: "definitely-not-here-xyz", UsedBy: "test"},
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if !results[0].OK {
		t.Errorf("expected fakecmd to be found, got: %s", results[0].Detail)
	}
	if results[1].OK {
		t.Errorf("expected missing binary to fail, got: %s", results[1].Detail)
	}
	if !strings.Contains(results[1].Detail, "not in PATH") {
		t.Errorf("expected a 'not in PATH' detail, got: %s", results[1].Detail)
	}
}

// promStub answers /api/v1/query with the canned payload for the exact
// query text, empty-success otherwise.
func promStub(t *testing.T, responses map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if resp, ok := responses[q]; ok {
			_, _ = w.Write([]byte(resp))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	promAliveOK = `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"__name__":"up"},"value":[1,"1"]}]}}`
	promScrapeOK = `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"job":"ceph"},"value":[1,"1"]}]}}`
)

func TestPromChecksAllPass(t *testing.T) {
	srv := promStub(t, map[string]string{
		"up":                             promAliveOK,
		`up{cluster='c1',instance='h1'}`: promScrapeOK,
		`count(ceph_bluestore_slow_committed_kv_count{cluster='c1',instance='h1'})`: `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"5"]}]}}`,
		`count(ceph_bluestore_slow_committed_kv_count{cluster='c1'})`:               `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"42"]}]}}`,
	})

	results := promChecks(&config.PrometheusConfig{URL: srv.URL, ClusterID: "c1"}, prometheus.NewClient(), "h1")
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("expected OK, got FAIL: %s", r.Detail)
		}
	}
	if !strings.Contains(results[2].Detail, "5 series") {
		t.Errorf("expected host series count in detail, got: %s", results[2].Detail)
	}
}

func TestPromChecksHostNotScraped(t *testing.T) {
	srv := promStub(t, map[string]string{
		"up":                             promAliveOK,
		`up{cluster='c1',instance='h1'}`: promScrapeOK,
		`count(ceph_bluestore_slow_committed_kv_count{cluster='c1'})`: `{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"42"]}]}}`,
	})

	results := promChecks(&config.PrometheusConfig{URL: srv.URL, ClusterID: "c1"}, prometheus.NewClient(), "h1")
	last := results[len(results)-1]
	if last.OK {
		t.Fatalf("expected metric check to fail, got: %s", last.Detail)
	}
	if !strings.Contains(last.Detail, "hostname mismatch") {
		t.Errorf("expected a hostname-mismatch diagnosis, got: %s", last.Detail)
	}
}

func TestPromChecksMetricMissingClusterWide(t *testing.T) {
	srv := promStub(t, map[string]string{
		"up":                             promAliveOK,
		`up{cluster='c1',instance='h1'}`: promScrapeOK,
	})

	results := promChecks(&config.PrometheusConfig{URL: srv.URL, ClusterID: "c1"}, prometheus.NewClient(), "h1")
	last := results[len(results)-1]
	if last.OK {
		t.Fatalf("expected metric check to fail, got: %s", last.Detail)
	}
	if !strings.Contains(last.Detail, "missing cluster-wide") {
		t.Errorf("expected a missing-cluster-wide diagnosis, got: %s", last.Detail)
	}
}

func TestPromChecksEndpointDown(t *testing.T) {
	// Close immediately: every connection fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	results := promChecks(&config.PrometheusConfig{URL: srv.URL, ClusterID: "c1"}, prometheus.NewClient(), "h1")
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected a single failed 'alive' check, got: %+v", results)
	}
	if !strings.Contains(results[0].Detail, "unreachable") {
		t.Errorf("expected an 'unreachable' detail, got: %s", results[0].Detail)
	}
}

// webhookCapture records the JSON bodies POSTed to the stub webhook.
func webhookCapture(t *testing.T) (*httptest.Server, *[]string) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// TestWebhookChecksFallback covers the production shape: webhook + channel
// + error_channel, no error_webhook - both messages go to the regular
// webhook, the error one with error_channel routing.
func TestWebhookChecksFallback(t *testing.T) {
	srv, bodies := webhookCapture(t)

	cfg := &config.Config{Notifications: &config.NotificationConfig{Webhook: srv.URL, Channel: "chan", ErrorChannel: "err-chan"}}
	results := webhookChecks(cfg, "h1")
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("expected two passing webhook checks, got: %+v", results)
	}
	if len(*bodies) != 2 {
		t.Fatalf("expected 2 POSTs to the regular webhook, got %d: %v", len(*bodies), *bodies)
	}
	if !strings.Contains((*bodies)[0], "action test") {
		t.Errorf("expected first message to be the action test, got: %s", (*bodies)[0])
	}
	if !strings.Contains((*bodies)[1], "error test") {
		t.Errorf("expected second message to be the error test, got: %s", (*bodies)[1])
	}
	if !strings.Contains((*bodies)[1], `"channel":"err-chan"`) {
		t.Errorf("expected error_channel routing in the error message, got: %s", (*bodies)[1])
	}
	if !strings.Contains(results[1].Detail, "error_webhook not set") {
		t.Errorf("expected the fallback note in the detail, got: %s", results[1].Detail)
	}
}

// TestWebhookChecksErrorRouting verifies SendError targets the dedicated
// error webhook when one is configured.
func TestWebhookChecksErrorRouting(t *testing.T) {
	actionSrv, actionBodies := webhookCapture(t)
	errorSrv, errorBodies := webhookCapture(t)

	cfg := &config.Config{Notifications: &config.NotificationConfig{
		Webhook: actionSrv.URL, Channel: "chan", ErrorChannel: "err-chan", ErrorWebhook: errorSrv.URL,
	}}
	results := webhookChecks(cfg, "h1")
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("expected two passing webhook checks, got: %+v", results)
	}
	if len(*actionBodies) != 1 || !strings.Contains((*actionBodies)[0], "action test") {
		t.Errorf("expected the action message on the regular webhook, got: %v", *actionBodies)
	}
	if len(*errorBodies) != 1 || !strings.Contains((*errorBodies)[0], "error test") {
		t.Errorf("expected the error message on the error webhook, got: %v", *errorBodies)
	}
	if !strings.Contains(results[1].Detail, "err-chan") || strings.Contains(results[1].Detail, "error_webhook not set") {
		t.Errorf("expected the dedicated-error-target detail, got: %s", results[1].Detail)
	}
}

func TestWebhookChecksFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Notifications: &config.NotificationConfig{Webhook: srv.URL, Channel: "test-chan"}}
	results := webhookChecks(cfg, "h1")
	if len(results) != 2 || results[0].OK || results[1].OK {
		t.Fatalf("expected two failing webhook checks, got: %+v", results)
	}
	if !strings.Contains(results[0].Detail, "failed") || !strings.Contains(results[1].Detail, "failed") {
		t.Errorf("expected failure details, got: %s / %s", results[0].Detail, results[1].Detail)
	}
}

func TestWebhookChecksUnconfigured(t *testing.T) {
	results := webhookChecks(&config.Config{}, "h1")
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected an unconfigured-notification failure, got: %+v", results)
	}
	if !strings.Contains(results[0].Detail, "not configured") {
		t.Errorf("expected a 'not configured' detail, got: %s", results[0].Detail)
	}
}
