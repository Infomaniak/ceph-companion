package operator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infomaniak/ceph-companion/internal/ceph"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// webhookCapture records the path (tokenizing which webhook was hit) and
// the decoded payload of every POST.
type webhookCapture struct {
	got    map[string]string // path suffix -> message text
	server *httptest.Server
}

func newWebhookCapture(t *testing.T) *webhookCapture {
	t.Helper()
	cap := &webhookCapture{got: map[string]string{}}
	mux := http.NewServeMux()
	handle := func(label string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				Channel string `json:"channel"`
				Text    string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			cap.got[label] = payload.Channel + "|" + payload.Text
			w.WriteHeader(http.StatusOK)
		}
	}
	mux.HandleFunc("/hooks/main", handle("main"))
	mux.HandleFunc("/hooks/errors", handle("errors"))
	cap.server = httptest.NewServer(mux)
	t.Cleanup(cap.server.Close)
	return cap
}

// A dedicated error channel must receive execution-error notifications,
// while regular notifications stay on the main channel.
func TestSendErrorUsesDedicatedChannel(t *testing.T) {
	cap := newWebhookCapture(t)
	nc := NewNotificationClient(&config.NotificationConfig{
		Channel:      "quiet-alarms",
		Webhook:      cap.server.URL + "/hooks/main",
		ErrorChannel: "spamy-errors",
		ErrorWebhook: cap.server.URL + "/hooks/errors",
	})

	if err := nc.Send("action taken"); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if err := nc.SendError("execution errors"); err != nil {
		t.Fatalf("SendError failed: %v", err)
	}

	if !strings.HasPrefix(cap.got["main"], "quiet-alarms|action taken") {
		t.Errorf("regular notification went to the wrong target: %q", cap.got["main"])
	}
	if !strings.HasPrefix(cap.got["errors"], "spamy-errors|execution errors") {
		t.Errorf("error notification went to the wrong target: %q", cap.got["errors"])
	}
}

// Without error_channel/error_webhook configured, error notifications fall
// back to the regular webhook/channel so the feature works out of the box.
func TestSendErrorFallsBackToMainWebhook(t *testing.T) {
	cap := newWebhookCapture(t)
	nc := NewNotificationClient(&config.NotificationConfig{
		Channel: "quiet-alarms",
		Webhook: cap.server.URL + "/hooks/main",
	})

	if err := nc.SendError("execution errors"); err != nil {
		t.Fatalf("SendError failed: %v", err)
	}
	if !strings.HasPrefix(cap.got["main"], "quiet-alarms|execution errors") {
		t.Errorf("expected fallback to the main webhook/channel, got %q", cap.got["main"])
	}
	if msg, ok := cap.got["errors"]; ok {
		t.Errorf("nothing should hit the error webhook, got %q", msg)
	}
}

func TestBuildErrorNotification(t *testing.T) {
	msg := buildErrorNotification("osd-host-1", "live", []string{
		"rule_a / osd_ok_to_stop: osd.5: ok-to-stop check failed: exec not found",
		"rule_b / action: command failed on osd.7: exit 1",
	})
	for _, want := range []string{
		"**ceph-companion execution errors**",
		"**Host:** osd-host-1",
		"**Mode:** live",
		"- rule_a / osd_ok_to_stop: osd.5: ok-to-stop check failed: exec not found",
		"- rule_b / action: command failed on osd.7: exit 1",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q, got:\n%s", want, msg)
		}
	}
}

func TestValidateRuleUnknownCommand(t *testing.T) {
	err := validateRule(&Rule{
		Name: "typo",
		Conditions: []RuleCondition{
			{Command: "prometheus_querry"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown condition command") {
		t.Fatalf("expected unknown-command error, got %v", err)
	}

	// A known command must pass (threshold requirements aside).
	if err := validateRule(&Rule{Name: "ok", Conditions: []RuleCondition{{Command: "osd_ok_to_stop"}}}); err != nil {
		t.Errorf("expected known command to validate, got %v", err)
	}
}

// The original blind-spot scenario: cephadm missing from PATH must turn
// into an explicit error (notified via kChat), not into a silent
// "already stopped"/"not safe" that looks like a healthy no-match.
func TestEvalOSDOkToStopMissingCephadm(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	eng := &Engine{CephClient: ceph.NewClient()}
	result := eng.evalOSDOkToStop(map[string]interface{}{
		"input_items": []Item{{Type: "osd", ID: "950"}},
	}, &RuleCondition{})

	if len(result.Errors) == 0 {
		t.Fatalf("expected execution errors for a missing cephadm, got %+v", result)
	}
	joined := strings.Join(result.Errors, "\n")
	if !strings.Contains(joined, "osd.950") || !strings.Contains(joined, "cephadm") {
		t.Errorf("expected the error to name the OSD and cephadm, got: %s", joined)
	}
	if result.Triggered {
		t.Error("a failed check must not trigger the action")
	}
}

// A Prometheus that cannot answer (HTTP 500) must surface as an execution
// error on the condition, not as a silent no-trigger.
func TestEvalPrometheusQueryServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	eng := &Engine{PromClient: prometheus.NewClient(), Config: prometheusConfig(srv.URL)}
	threshold := 1.0
	result := eng.evalPrometheusQuery(map[string]interface{}{}, &RuleCondition{
		Query:     "up",
		Operator:  ">",
		Threshold: &threshold,
	})

	if len(result.Errors) == 0 {
		t.Fatalf("expected execution errors for a failing Prometheus, got %+v", result)
	}
	if !strings.Contains(strings.Join(result.Errors, "\n"), "prometheus query failed") {
		t.Errorf("expected a query-failed error, got: %v", result.Errors)
	}
}
