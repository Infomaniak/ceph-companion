package operator

import (
	"fmt"
	"strings"

	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// Commands: osd_performance_drop, pool_performance_drop, rgw_performance_drop.
//
// Each is a cluster-wide gate comparing a 5m rate against a 1h rate across
// two or three independent metrics (OR'd together), which isn't expressible
// as a single PromQL comparison - hence dedicated functions instead of
// prometheus_query. None of them identify a specific entity, so they always
// return EvalResult.Items == nil (see engine.go's evalRule: a triggered
// condition with no items of its own is treated as a pure gate and doesn't
// clear whatever items an earlier condition already found).

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// dropEval carries the setup shared by the *_performance_drop conditions:
// the Prometheus endpoint config, auth, query URL and the rule's threshold.
type dropEval struct {
	e         *Engine
	p         *config.PrometheusConfig
	auth      *prometheus.Auth
	baseURL   string
	threshold float64
}

// newDropEval resolves the Prometheus config and the condition's threshold.
// The threshold is guaranteed non-nil here: engine.go's validateRule refuses
// to run a rule with a *_performance_drop condition that omits it, rather
// than picking a hidden default (a 0% threshold would make this trigger on
// any noise, and it gates OSD-stop actions).
func (e *Engine) newDropEval(cond *RuleCondition) (*dropEval, *EvalResult) {
	p, err := e.promConfig()
	if err != nil {
		return nil, &EvalResult{Triggered: false, Errors: []string{err.Error()}}
	}
	return &dropEval{
		e:         e,
		p:         p,
		auth:      prometheus.NewAuth(p.Username, p.Password),
		baseURL:   p.QueryURL(),
		threshold: *cond.Threshold,
	}, nil
}

// selector returns the cluster scoping selector.
func (ev *dropEval) selector() string {
	return prometheus.BuildSelector([2]string{ev.p.LabelName(), ev.p.ClusterID})
}

// queryAll runs the named PromQL queries, stopping at the first failure or
// empty result. kind prefixes the error messages (e.g. "RGW ").
func (ev *dropEval) queryAll(kind string, queries map[string]string) (map[string]float64, []string) {
	results := make(map[string]float64, len(queries))
	var errs []string
	for name, q := range queries {
		r, err := ev.e.PromClient.Query(ev.baseURL, q, ev.auth)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s%s query failed: %v (Prometheus: %s)", kind, name, err, ev.p.URL))
			break
		}
		if len(r) == 0 {
			errs = append(errs, fmt.Sprintf("No %s%s data (Prometheus: %s) - metric missing or scrape broken.", kind, name, ev.p.URL))
			break
		}
		results[name] = prometheus.ExtractValue(r[0])
	}
	return results, errs
}

// EvalOSDPerformanceDrop evaluates OSD performance drop on the configured
// Prometheus endpoint.
func (e *Engine) EvalOSDPerformanceDrop(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	ev, fail := e.newDropEval(cond)
	if fail != nil {
		return fail
	}
	selector := ev.selector()
	queries := map[string]string{
		"IOPS 5m": fmt.Sprintf("sum(irate(ceph_osd_op_w%s[5m]) + irate(ceph_osd_op_r%s[5m]))", selector, selector),
		"IOPS 1h": fmt.Sprintf("sum(rate(ceph_osd_op_w%s[1h]) + irate(ceph_osd_op_r%s[1h]))", selector, selector),
		"BW 5m":   fmt.Sprintf("sum(irate(ceph_osd_op_w_in_bytes%s[5m]) + irate(ceph_osd_op_r_out_bytes%s[5m]))", selector, selector),
		"BW 1h":   fmt.Sprintf("sum(rate(ceph_osd_op_w_in_bytes%s[1h]) + irate(ceph_osd_op_r_out_bytes%s[1h]))", selector, selector),
	}

	results, errs := ev.queryAll("", queries)
	if len(errs) > 0 {
		return &EvalResult{Triggered: false, Errors: errs}
	}

	iopsDrop := prometheus.ComputeDrop(results["IOPS 5m"], results["IOPS 1h"])
	bwDrop := prometheus.ComputeDrop(results["BW 5m"], results["BW 1h"])

	logs := []string{
		fmt.Sprintf("\nPrometheus: %s (cluster_id: %s)", ev.p.URL, ev.p.ClusterID),
		"Performance (5m vs 1h), positive diff = traffic increased:",
		fmt.Sprintf("  IOPS: %.2f (5m) vs %.2f (1h) → diff: %+.2f%%", results["IOPS 5m"], results["IOPS 1h"], iopsDrop),
		fmt.Sprintf("  BW: %s (5m) vs %s (1h) → diff: %+.2f%%",
			prometheus.FormatRate(results["BW 5m"], "bytes"),
			prometheus.FormatRate(results["BW 1h"], "bytes"),
			bwDrop),
	}

	if iopsDrop < -ev.threshold || bwDrop < -ev.threshold {
		logs = append(logs, "Significant drop detected!")
		return &EvalResult{Triggered: true, Logs: logs}
	}
	logs = append(logs, fmt.Sprintf("Performance stable (triggers on diff < -%.2f%%; negative threshold forces a trigger for testing).", ev.threshold))
	return &EvalResult{Triggered: false, Logs: logs}
}

// EvalPoolPerformanceDrop evaluates pool performance drop.
func (e *Engine) EvalPoolPerformanceDrop(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	ev, fail := e.newDropEval(cond)
	if fail != nil {
		return fail
	}

	triggered := false
	logs := []string{
		fmt.Sprintf("\nPrometheus: %s", ev.p.URL),
		fmt.Sprintf("Found cluster_id: %s", ev.p.ClusterID),
	}

	label := ev.p.LabelName()
	writeQuery := fmt.Sprintf("ceph_pool_wr_bytes%s", prometheus.BuildSelector([2]string{label, ev.p.ClusterID}))
	writeResults, err := e.PromClient.Query(ev.baseURL, writeQuery, ev.auth)
	if err != nil {
		return &EvalResult{Triggered: triggered, Logs: logs, Errors: []string{fmt.Sprintf("pool write query failed: %v (Prometheus: %s)", err, ev.p.URL)}}
	}
	if len(writeResults) == 0 {
		return &EvalResult{Triggered: triggered, Logs: logs, Errors: []string{fmt.Sprintf("No write data for any pool (Prometheus: %s) - metric missing or scrape broken.", ev.p.URL)}}
	}

	readQuery := fmt.Sprintf("ceph_pool_rd_bytes%s", prometheus.BuildSelector([2]string{label, ev.p.ClusterID}))
	readResults, err := e.PromClient.Query(ev.baseURL, readQuery, ev.auth)
	if err != nil {
		return &EvalResult{Triggered: triggered, Logs: logs, Errors: []string{fmt.Sprintf("pool read query failed: %v (Prometheus: %s)", err, ev.p.URL)}}
	}

	poolData := make(map[string]map[string]float64)
	for _, item := range writeResults {
		pid := item.Metric["pool_id"]
		if pid == "" {
			continue
		}
		if poolData[pid] == nil {
			poolData[pid] = make(map[string]float64)
		}
		poolData[pid]["write"] = prometheus.ExtractValue(item)
	}
	for _, item := range readResults {
		pid := item.Metric["pool_id"]
		if pid == "" {
			continue
		}
		if poolData[pid] == nil {
			poolData[pid] = make(map[string]float64)
		}
		poolData[pid]["read"] = prometheus.ExtractValue(item)
	}

	for pid := range poolData {
		metrics := e.getPoolMetrics(ev.baseURL, label, ev.p.ClusterID, pid, ev.auth)
		if metrics == nil {
			continue
		}

		writeDrop := prometheus.ComputeDrop(metrics["write_5m"], metrics["write_1h"])
		readDrop := prometheus.ComputeDrop(metrics["read_5m"], metrics["read_1h"])
		totalDrop := prometheus.ComputeDrop(metrics["write_5m"]+metrics["read_5m"], metrics["write_1h"]+metrics["read_1h"])

		logs = append(logs, fmt.Sprintf("\nPool: %s", pid))
		logs = append(logs, fmt.Sprintf("  Write: %s (5m) vs %s (1h) → diff: %+.2f%%",
			prometheus.FormatRate(metrics["write_5m"], "bytes"),
			prometheus.FormatRate(metrics["write_1h"], "bytes"),
			writeDrop))
		if metrics["read_5m"] > 0 {
			logs = append(logs, fmt.Sprintf("  Read: %s (5m) vs %s (1h) → diff: %+.2f%%",
				prometheus.FormatRate(metrics["read_5m"], "bytes"),
				prometheus.FormatRate(metrics["read_1h"], "bytes"),
				readDrop))
		}
		logs = append(logs, fmt.Sprintf("  Total: %s (5m) vs %s (1h) → diff: %+.2f%%",
			prometheus.FormatRate(metrics["write_5m"]+metrics["read_5m"], "bytes"),
			prometheus.FormatRate(metrics["write_1h"]+metrics["read_1h"], "bytes"),
			totalDrop))

		if writeDrop < -ev.threshold || readDrop < -ev.threshold || totalDrop < -ev.threshold {
			logs = append(logs, "Significant drop detected!")
			triggered = true
		} else {
			logs = append(logs, fmt.Sprintf("Performance stable (triggers on diff < -%.2f%%; negative threshold forces a trigger for testing).", ev.threshold))
		}
	}

	return &EvalResult{Triggered: triggered, Logs: logs}
}

func (e *Engine) getPoolMetrics(baseURL, labelName, clusterID, poolID string, auth *prometheus.Auth) map[string]float64 {
	poolSelector := prometheus.BuildSelector([2]string{labelName, clusterID}, [2]string{"pool_id", poolID})

	queries := map[string]string{
		"write_5m": fmt.Sprintf("irate(ceph_pool_wr_bytes%s[5m])", poolSelector),
		"write_1h": fmt.Sprintf("rate(ceph_pool_wr_bytes%s[1h])", poolSelector),
		"read_5m":  fmt.Sprintf("irate(ceph_pool_rd_bytes%s[5m])", poolSelector),
		"read_1h":  fmt.Sprintf("rate(ceph_pool_rd_bytes%s[1h])", poolSelector),
	}

	results := make(map[string]float64)
	for name, q := range queries {
		r, err := e.PromClient.Query(baseURL, q, auth)
		if err != nil || len(r) == 0 {
			if strings.Contains(name, "write") {
				return nil
			}
			results[name] = 0
			continue
		}
		results[name] = prometheus.ExtractValue(r[0])
	}
	return results
}

// EvalRGWPerformanceDrop evaluates RGW performance drop.
func (e *Engine) EvalRGWPerformanceDrop(kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	ev, fail := e.newDropEval(cond)
	if fail != nil {
		return fail
	}
	selector := ev.selector()
	queries := map[string]string{
		"put_5m": fmt.Sprintf("sum(irate(ceph_rgw_op_put_obj_ops%s[5m]))", selector),
		"put_1h": fmt.Sprintf("sum(rate(ceph_rgw_op_put_obj_ops%s[1h]))", selector),
		"get_5m": fmt.Sprintf("sum(irate(ceph_rgw_op_get_obj_ops%s[5m]))", selector),
		"get_1h": fmt.Sprintf("sum(rate(ceph_rgw_op_get_obj_ops%s[1h]))", selector),
	}

	results, errs := ev.queryAll("RGW ", queries)
	if len(errs) > 0 {
		return &EvalResult{Triggered: false, Errors: errs}
	}

	total5m := results["put_5m"] + results["get_5m"]
	total1h := results["put_1h"] + results["get_1h"]

	drops := map[string]float64{
		"put":   prometheus.ComputeDrop(results["put_5m"], results["put_1h"]),
		"get":   prometheus.ComputeDrop(results["get_5m"], results["get_1h"]),
		"total": prometheus.ComputeDrop(total5m, total1h),
	}

	logs := []string{
		fmt.Sprintf("\nPrometheus: %s (cluster_id: %s)", ev.p.URL, ev.p.ClusterID),
		"RGW I/O (5m vs 1h), positive diff = traffic increased:",
	}
	for _, op := range []string{"put", "get"} {
		v5 := prometheus.FormatRate(results[op+"_5m"], "queries")
		v1 := prometheus.FormatRate(results[op+"_1h"], "queries")
		logs = append(logs, fmt.Sprintf("  %s: %s (5m) vs %s (1h) → diff: %+.2f%%",
			capitalize(op), v5, v1, drops[op]))
	}
	logs = append(logs, fmt.Sprintf("  Total: %s (5m) vs %s (1h) → diff: %+.2f%%",
		prometheus.FormatRate(total5m, "queries"),
		prometheus.FormatRate(total1h, "queries"),
		drops["total"]))

	if drops["put"] < -ev.threshold || drops["get"] < -ev.threshold || drops["total"] < -ev.threshold {
		logs = append(logs, "Significant drop detected!")
		return &EvalResult{Triggered: true, Logs: logs}
	}
	logs = append(logs, fmt.Sprintf("Performance stable (triggers on diff < -%.2f%%; negative threshold forces a trigger for testing).", ev.threshold))
	return &EvalResult{Triggered: false, Logs: logs}
}
