package cli

import (
	"os"
	"testing"
)

// The embedded sample and the shipped example must never drift apart.
func TestSampleRuleSync(t *testing.T) {
	etcRule, err := os.ReadFile("../../etc/ceph-companion/rules/stop_sluggish_osd_service.yaml")
	if err != nil {
		t.Fatalf("reading shipped sample rule: %v", err)
	}
	if string(etcRule) != sampleRule {
		t.Error("internal/cli/sample_rule.yaml has drifted from " +
			"etc/ceph-companion/rules/stop_sluggish_osd_service.yaml - copy one over the other")
	}
}
