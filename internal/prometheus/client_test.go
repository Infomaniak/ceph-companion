package prometheus

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewAuth(t *testing.T) {
	if NewAuth("", "secret") != nil {
		t.Error("expected nil auth for empty username")
	}
	if auth := NewAuth("user", "secret"); auth == nil || auth.Username != "user" || auth.Password != "secret" {
		t.Errorf("unexpected auth: %+v", auth)
	}
}

func TestQueryBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var sawAuthHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, sawAuthHeader = r.BasicAuth()
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	client := NewClient()
	if _, err := client.Query(srv.URL+"/api/v1/query", "up", nil); err != nil {
		t.Fatalf("query without auth failed: %v", err)
	}
	if sawAuthHeader {
		t.Error("expected no Authorization header without auth")
	}

	if _, err := client.Query(srv.URL+"/api/v1/query", "up", NewAuth("monitoring", "secret")); err != nil {
		t.Fatalf("query with auth failed: %v", err)
	}
	if !sawAuthHeader || gotUser != "monitoring" || gotPass != "secret" {
		t.Errorf("expected basic auth credentials, got user=%q pass=%q header=%v", gotUser, gotPass, sawAuthHeader)
	}
}

// TestQueryPreservesSubpath pins the URL handling for endpoints that serve
// the Prometheus API under a subpath - e.g. a VictoriaMetrics proxy exposing
// /prometheus/api/v1/query. Whatever subpath the configured URL carries must
// reach the server, and the /api/v1/query suffix must not be duplicated.
func TestQueryPreservesSubpath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)

	client := NewClient()

	// Full query URL with a subpath (what QueryURL produces for a config
	// url like "https://host/prometheus").
	if _, err := client.Query(srv.URL+"/prometheus/api/v1/query", "up", nil); err != nil {
		t.Fatalf("query with subpath failed: %v", err)
	}
	if gotPath != "/prometheus/api/v1/query" {
		t.Errorf("subpath not preserved: got %q", gotPath)
	}

	// Bare base URL (no path) still resolves to the query endpoint.
	if _, err := client.Query(srv.URL, "up", nil); err != nil {
		t.Fatalf("query without path failed: %v", err)
	}
	if gotPath != "/api/v1/query" {
		t.Errorf("expected /api/v1/query appended, got %q", gotPath)
	}
}
