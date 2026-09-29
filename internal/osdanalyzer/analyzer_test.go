package osdanalyzer

import (
	"strings"
	"testing"
)

func TestLoadPattern(t *testing.T) {
	// Test loading a non-existent file
	_, err := LoadPattern("nonexistent.yaml")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestEvaluatePatternTOP(t *testing.T) {
	data := map[string]interface{}{
		"ops": []interface{}{
			map[string]interface{}{"duration": 100.0, "type": "test1", "description": "desc1"},
			map[string]interface{}{"duration": 50.0, "type": "test2", "description": "desc2"},
			map[string]interface{}{"duration": 200.0, "type": "test3", "description": "desc3"},
		},
	}

	// Test TOP(2, ops)
	result := EvaluatePattern("TOP(2, ops)", data, "historic_ops")
	if !result.Success {
		t.Errorf("expected TOP(2, ops) to succeed")
	}
	topOps, ok := result.FoundValues["top_ops"].([]map[string]interface{})
	if !ok || len(topOps) != 2 {
		t.Errorf("expected 2 top ops, got %v", result.FoundValues["top_ops"])
	}
}

func TestEvaluatePatternCount(t *testing.T) {
	data := map[string]interface{}{
		"ops": []interface{}{
			map[string]interface{}{"duration": 10.0},
			map[string]interface{}{"duration": 20.0},
			map[string]interface{}{"duration": 30.0},
		},
	}

	// Test count(events) > 2
	result := EvaluatePattern("count(events) > 2", data, "historic_ops")
	if !result.Success {
		t.Errorf("expected count(3) > 2 to succeed, got %v", result.Success)
	}
}

func TestEvaluatePatternEventDuration(t *testing.T) {
	data := map[string]interface{}{
		"ops": []interface{}{
			map[string]interface{}{
				"type_data": map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{"event": "commit", "duration": 150.0},
						map[string]interface{}{"event": "apply", "duration": 50.0},
					},
				},
				"description": "test op",
			},
		},
	}

	// Test event_duration('commit') > 100
	result := EvaluatePattern("event_duration('commit') > 100", data, "historic_ops")
	if !result.Success {
		t.Errorf("expected event_duration > 100 to succeed, got %v", result.Success)
	}
}

func TestDeepGet(t *testing.T) {
	data := map[string]interface{}{
		"level1": map[string]interface{}{
			"level2": map[string]interface{}{
				"value": 42.0,
			},
		},
	}

	val := deepGet(data, "level1.level2.value")
	if val == nil {
		t.Fatal("expected value, got nil")
	}
	if v, ok := val.(float64); !ok || v != 42.0 {
		t.Errorf("expected 42.0, got %v", val)
	}

	// Test missing path
	val = deepGet(data, "nonexistent.path")
	if val != nil {
		t.Errorf("expected nil for missing path, got %v", val)
	}
}

func TestEvaluateSimpleExpression(t *testing.T) {
	tests := []struct {
		expr     string
		expected bool
	}{
		{"100 > 50", true},
		{"50 > 100", false},
		{"100 >= 100", true},
		{"100 < 200", true},
		{"100 == 100", true},
		{"100 != 100", false},
	}

	for _, test := range tests {
		result := evaluateSimpleExpression(test.expr)
		if result != test.expected {
			t.Errorf("evaluateSimpleExpression(%q) = %v, expected %v", test.expr, result, test.expected)
		}
	}
}

func TestEvaluatePatternPathVsPath(t *testing.T) {
	data := map[string]interface{}{
		"osd_scrub_dp_ec": []interface{}{
			map[string]interface{}{
				"counters": map[string]interface{}{
					"num_scrubs_started": 5.0,
					"successful_scrubs":  3.0,
				},
			},
		},
	}

	result := EvaluatePattern(
		"osd_scrub_dp_ec[0].counters.num_scrubs_started > osd_scrub_dp_ec[0].counters.successful_scrubs",
		data, "osd_counter_dump",
	)
	if !result.Success {
		t.Errorf("expected 5 > 3 (path vs path) to succeed, got %v (%s)", result.Success, result.Message)
	}

	result = EvaluatePattern(
		"osd_scrub_dp_ec[0].counters.successful_scrubs > osd_scrub_dp_ec[0].counters.num_scrubs_started",
		data, "osd_counter_dump",
	)
	if result.Success {
		t.Errorf("expected 3 > 5 (path vs path) to fail, got %v", result.Success)
	}
}

func TestEvaluateGenericExpressionMissingMetric(t *testing.T) {
	data := map[string]interface{}{"foo": map[string]interface{}{"bar": 1.0}}

	result := EvaluatePattern("foo.bar > foo.missing", data, "osd_counter_dump")
	if result.Success {
		t.Errorf("expected evaluation to be skipped (not silently compared against 0) when a metric is missing")
	}
	if result.FoundValues["foo.missing"] != "no data found" {
		t.Errorf("expected missing metric to be flagged, got %v", result.FoundValues["foo.missing"])
	}
}

// osd156CounterDumpFixture mirrors the real `ceph tell osd.N counter dump`
// shape observed on OSD.156: scrub bookkeeping counters live in the osd
// subsystem (scalars plus {avgcount,sum,avgtime} elapsed maps), chunk-level
// counters in the osd_scrub_* subsystems.
func osd156CounterDumpFixture() map[string]interface{} {
	return map[string]interface{}{
		"osd": []interface{}{
			map[string]interface{}{
				"labels": map[string]interface{}{},
				"counters": map[string]interface{}{
					"num_scrubs_started_ec":          170.0,
					"num_scrubs_past_reservation_ec": 163.0,
					"successful_scrubs_ec":           163.0,
					"failed_scrubs_ec":               0.0,
					"successful_scrubs_ec_elapsed": map[string]interface{}{
						"avgcount": 163.0,
						"sum":      2848051.703967302,
						"avgtime":  17472.709840290,
					},
					"num_scrubs_started_replicated": 0.0,
					"successful_scrubs_replicated":  0.0,
					"successful_scrubs_replicated_elapsed": map[string]interface{}{
						"avgcount": 0.0,
						"sum":      0.0,
						"avgtime":  0.0,
					},
				},
			},
		},
		"osd_scrub_dp_ec": []interface{}{
			map[string]interface{}{
				"labels": map[string]interface{}{"level": "deep", "pooltype": "ec"},
				"counters": map[string]interface{}{
					"preemptions":            1.0,
					"chunk_selected":         17189.0,
					"chunk_busy":             0.0,
					"locked_object":          0.0,
					"write_blocked_by_scrub": 0.0,
				},
			},
		},
	}
}

func TestResolveCephTokenAvgtime(t *testing.T) {
	data := osd156CounterDumpFixture()

	// Newer Ceph emits a literal avgtime key.
	v, ok := resolveCephToken(data, "osd[0].counters.successful_scrubs_ec_elapsed.avgtime")
	if !ok || v != 17472.709840290 {
		t.Errorf("expected literal avgtime 17472.709840290, got %v (ok=%v)", v, ok)
	}

	// Older daemons only emit {avgcount, sum}: avgtime must be computed.
	older := map[string]interface{}{
		"osd": []interface{}{
			map[string]interface{}{
				"counters": map[string]interface{}{
					"successful_scrubs_ec_elapsed": map[string]interface{}{
						"avgcount": 163.0,
						"sum":      2848051.703967302,
					},
				},
			},
		},
	}
	v, ok = resolveCephToken(older, "osd[0].counters.successful_scrubs_ec_elapsed.avgtime")
	if !ok || v != 2848051.703967302/163.0 {
		t.Errorf("expected computed sum/avgcount %v, got %v (ok=%v)", 2848051.703967302/163.0, v, ok)
	}

	// avgcount == 0 means nothing has been averaged yet: unresolvable, even
	// though the daemon emits a literal avgtime of 0.
	v, ok = resolveCephToken(data, "osd[0].counters.successful_scrubs_replicated_elapsed.avgtime")
	if ok {
		t.Errorf("expected avgtime with avgcount=0 to be unresolvable, got %v", v)
	}

	// Missing path.
	if _, ok := resolveCephToken(data, "osd[0].counters.no_such_counter.avgtime"); ok {
		t.Errorf("expected missing counter to be unresolvable")
	}
}

func TestResolveCephTokenBareCounter(t *testing.T) {
	data := osd156CounterDumpFixture()

	// Bare map-typed counter resolves to the cumulative sum.
	v, ok := resolveCephToken(data, "osd[0].counters.successful_scrubs_ec_elapsed")
	if !ok || v != 2848051.703967302 {
		t.Errorf("expected bare counter map to resolve to sum 2848051.703967302, got %v (ok=%v)", v, ok)
	}

	// Scalars resolve as-is.
	v, ok = resolveCephToken(data, "osd[0].counters.num_scrubs_started_ec")
	if !ok || v != 170.0 {
		t.Errorf("expected scalar counter 170, got %v (ok=%v)", v, ok)
	}

	// Literal avgcount/sum suffixes keep working via deepGet.
	v, ok = resolveCephToken(data, "osd[0].counters.successful_scrubs_ec_elapsed.avgcount")
	if !ok || v != 163.0 {
		t.Errorf("expected literal avgcount 163, got %v (ok=%v)", v, ok)
	}
}

func TestEvaluatePatternCounterDumpFixture(t *testing.T) {
	data := osd156CounterDumpFixture()

	// Real values from OSD.156: avg scrub elapsed ~17472 s vs 2 h threshold.
	result := EvaluatePattern("osd[0].counters.successful_scrubs_ec_elapsed.avgtime > 7200", data, "osd_counter_dump")
	if !result.Success {
		t.Errorf("expected 17472.7 > 7200 to succeed, got %v (%s)", result.Success, result.Message)
	}
	if v := result.FoundValues["osd[0].counters.successful_scrubs_ec_elapsed.avgtime"]; v != 17472.709840290 {
		t.Errorf("expected resolved avgtime in FoundValues, got %v", v)
	}

	// started (170) > successful (163) => scrubs ongoing.
	result = EvaluatePattern(
		"osd[0].counters.num_scrubs_started_ec > osd[0].counters.successful_scrubs_ec",
		data, "osd_counter_dump",
	)
	if !result.Success {
		t.Errorf("expected 170 > 163 (path vs path) to succeed, got %v (%s)", result.Success, result.Message)
	}

	// Zero-scrub replicated elapsed must not evaluate (avgcount=0 => no data).
	result = EvaluatePattern("osd[0].counters.successful_scrubs_replicated_elapsed.avgtime > 7200", data, "osd_counter_dump")
	if result.Success {
		t.Errorf("expected replicated avgtime (avgcount=0) to skip evaluation, got %v", result.Success)
	}
	if result.FoundValues["osd[0].counters.successful_scrubs_replicated_elapsed.avgtime"] != "no data found" {
		t.Errorf("expected 'no data found' for zero-sample avgtime, got %v", result.FoundValues["osd[0].counters.successful_scrubs_replicated_elapsed.avgtime"])
	}

	// Chunk-level counters exist as scalars and evaluate numerically.
	result = EvaluatePattern("osd_scrub_dp_ec[0].counters.write_blocked_by_scrub > 1000", data, "osd_counter_dump")
	if result.Success {
		t.Errorf("expected 0 > 1000 to fail, got %v", result.Success)
	}
	if result.Message != "Pattern evaluated successfully" {
		t.Errorf("expected scalar chunk counter to resolve, got %s", result.Message)
	}
}

func TestEvaluatePatternLegacyScrubPathsMissing(t *testing.T) {
	// The pre-fix pattern paths don't exist on current Ceph: the
	// osd_scrub_* subsystems hold only chunk counters. They must be flagged
	// as missing, not silently compared against 0.
	data := osd156CounterDumpFixture()

	result := EvaluatePattern("osd_scrub_dp_ec[0].counters.num_scrubs_started > 0", data, "osd_counter_dump")
	if result.Success {
		t.Errorf("expected legacy path to be missing, got %v", result.Success)
	}
	if result.FoundValues["osd_scrub_dp_ec[0].counters.num_scrubs_started"] != "no data found" {
		t.Errorf("expected 'no data found' for legacy path, got %v", result.FoundValues["osd_scrub_dp_ec[0].counters.num_scrubs_started"])
	}
}

func TestEvaluatePatternScrubBlockedBy(t *testing.T) {
	blockedDesc := "osd_op(client.111 1.2 8:9e369d44:::.dir.abc12345-671f-4444-9999-000000000001.1.3:head [call] ondisk+write e100)"
	data := map[string]interface{}{
		"ops": []interface{}{
			map[string]interface{}{
				"type_data":   map[string]interface{}{"flag_point": "waiting for scrub"},
				"description": blockedDesc,
			},
			map[string]interface{}{
				"type_data":   map[string]interface{}{"flag_point": "waiting for scrub"},
				"description": blockedDesc,
			},
			map[string]interface{}{
				"type_data":   map[string]interface{}{"flag_point": "started"},
				"description": "osd_op(client.222 1.3 8:aaaaaaaa:::other-object:head [call] ondisk+write e100)",
			},
		},
	}

	result := EvaluatePattern("scrub_blocked_by(1) > 0", data, "osd_ops_in_flight")
	if !result.Success {
		t.Errorf("expected scrub_blocked_by(1) > 0 to succeed, got %v (%s)", result.Success, result.Message)
	}
	if result.FoundValues["scrub_total_blocked"] != 2 {
		t.Errorf("expected scrub_total_blocked=2, got %v", result.FoundValues["scrub_total_blocked"])
	}
	if result.FoundValues["scrub_top_count"] != 2 {
		t.Errorf("expected scrub_top_count=2, got %v", result.FoundValues["scrub_top_count"])
	}
	if result.FoundValues["scrub_bucket_type"] != "bucket_index" {
		t.Errorf("expected scrub_bucket_type=bucket_index, got %v", result.FoundValues["scrub_bucket_type"])
	}
	if result.FoundValues["scrub_bucket_uuid"] != "abc12345-671f-4444-9999-000000000001" {
		t.Errorf("expected scrub_bucket_uuid to match the object's uuid, got %v", result.FoundValues["scrub_bucket_uuid"])
	}
	if result.FoundValues["scrub_bucket_shard"] != "3" {
		t.Errorf("expected scrub_bucket_shard=3, got %v", result.FoundValues["scrub_bucket_shard"])
	}
}

// blockedOpsFixture returns two ops waiting for scrub on the same object
// (maxBlocked = 2).
func blockedOpsFixture() map[string]interface{} {
	blockedDesc := "osd_op(client.111 1.2 8:9e369d44:::.dir.abc12345-671f-4444-9999-000000000001.1.3:head [call] ondisk+write e100)"
	return map[string]interface{}{
		"ops": []interface{}{
			map[string]interface{}{
				"type_data":   map[string]interface{}{"flag_point": "waiting for scrub"},
				"description": blockedDesc,
			},
			map[string]interface{}{
				"type_data":   map[string]interface{}{"flag_point": "waiting for scrub"},
				"description": blockedDesc,
			},
		},
	}
}

func TestScrubBlockedByPerObjectThreshold(t *testing.T) {
	data := blockedOpsFixture()

	// maxBlocked = 2: threshold 2 fires, threshold 3 does not.
	if result := EvaluatePattern("scrub_blocked_by(2) > 0", data, "osd_ops_in_flight"); !result.Success {
		t.Errorf("expected scrub_blocked_by(2) > 0 to fire with 2 ops on one object, got %v (%s)", result.Success, result.Message)
	}
	result := EvaluatePattern("scrub_blocked_by(3) > 0", data, "osd_ops_in_flight")
	if result.Success {
		t.Errorf("expected scrub_blocked_by(3) > 0 not to fire with only 2 ops on one object, got %v", result.Success)
	}
	if !strings.Contains(result.Message, "per-object threshold of 3") {
		t.Errorf("expected threshold mention in message, got %s", result.Message)
	}

	// The gated value is 0, so == 0 checks work in the below-threshold case.
	if result := EvaluatePattern("scrub_blocked_by(3) == 0", data, "osd_ops_in_flight"); !result.Success {
		t.Errorf("expected scrub_blocked_by(3) == 0 to hold when below threshold, got %v (%s)", result.Success, result.Message)
	}

	// Argument 0 keeps the ungated (legacy) behavior.
	if result := EvaluatePattern("scrub_blocked_by(0) > 0", data, "osd_ops_in_flight"); !result.Success {
		t.Errorf("expected scrub_blocked_by(0) > 0 to fire whenever any op is blocked, got %v (%s)", result.Success, result.Message)
	}
}

func TestScrubBlockedByRequiresArgument(t *testing.T) {
	data := blockedOpsFixture()

	for _, pattern := range []string{"scrub_blocked_by() > 0", "scrub_blocked_by > 0", "scrub_blocked_by()"} {
		result := EvaluatePattern(pattern, data, "osd_ops_in_flight")
		if result.Success {
			t.Errorf("expected %q to be rejected, got Success", pattern)
		}
		if !strings.Contains(result.Message, "requires a numeric argument") {
			t.Errorf("expected clear rejection message for %q, got %s", pattern, result.Message)
		}
	}
}

func TestFormatOperation(t *testing.T) {
	op := map[string]interface{}{
		"duration":    150.5,
		"type":        "test_op",
		"description": "test description that is very long and should be truncated when it exceeds eighty characters so it gets shortened",
	}

	formatted := FormatOperation(op)
	if !strings.Contains(formatted, "150.50 ms") {
		t.Errorf("expected duration in output, got %s", formatted)
	}
	if len(formatted) < 80 { // Should be truncated
		t.Errorf("expected truncated output, got %s", formatted)
	}
}
