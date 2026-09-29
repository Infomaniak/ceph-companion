package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/operator"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

var (
	getDebug         bool
	getAlarm         bool
	getLatencyThresh float64
	getWindow        string
	getDropThreshold float64
)

func init() {
	getCmd := &cobra.Command{
		Use:   "get",
		Short: "Fetch metrics from Prometheus",
		Long:  `Fetch various Ceph metrics from Prometheus endpoints.`,
	}

	osdLatencyCmd := &cobra.Command{
		Use:   "osd-latency",
		Short: "Get OSD latency metrics",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGetCommand(cmd.Name(), getDebug, getAlarm, getLatencyThresh, getWindow)
		},
	}
	osdLatencyCmd.Flags().BoolVar(&getAlarm, "alarm", false, "Exit with code 3 for Zabbix if thresholds exceeded")
	osdLatencyCmd.Flags().Float64Var(&getLatencyThresh, "threshold", 1000, "Latency (ms) above which --alarm exits 3")

	slowOpsCmd := &cobra.Command{
		Use:   "slow-ops",
		Short: "Get Ceph slow operations health check",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGetCommand(cmd.Name(), getDebug, getAlarm, getLatencyThresh, getWindow)
		},
	}

	osdDropCmd := &cobra.Command{
		Use:   "osd-performance-drop",
		Short: "Check OSD performance drop",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDropCommand("osd", getDropThreshold)
		},
	}
	osdDropCmd.Flags().Float64Var(&getDropThreshold, "threshold", 30.0, "Percentage drop (5m vs 1h) considered significant")

	poolDropCmd := &cobra.Command{
		Use:   "pool-performance-drop",
		Short: "Check pool performance drop",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDropCommand("pool", getDropThreshold)
		},
	}
	poolDropCmd.Flags().Float64Var(&getDropThreshold, "threshold", 30.0, "Percentage drop (5m vs 1h) considered significant")

	rgwDropCmd := &cobra.Command{
		Use:   "rgw-performance-drop",
		Short: "Check RGW performance drop",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDropCommand("rgw", getDropThreshold)
		},
	}
	rgwDropCmd.Flags().Float64Var(&getDropThreshold, "threshold", 30.0, "Percentage drop (5m vs 1h) considered significant")

	bluestoreCmd := &cobra.Command{
		Use:   "bluestore-slow-committed-kv-count",
		Short: "Get BlueStore slow committed KV count",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGetCommand(cmd.Name(), getDebug, getAlarm, getLatencyThresh, getWindow)
		},
	}
	bluestoreCmd.Flags().StringVar(&getWindow, "window", "1h", "PromQL lookback window, e.g. 1h, 30m")

	for _, c := range []*cobra.Command{osdLatencyCmd, slowOpsCmd, osdDropCmd, poolDropCmd, rgwDropCmd, bluestoreCmd} {
		c.Flags().BoolVar(&getDebug, "debug", false, "Enable debug output")
	}

	getCmd.AddCommand(osdLatencyCmd, slowOpsCmd, osdDropCmd, poolDropCmd, rgwDropCmd, bluestoreCmd)
	rootCmd.AddCommand(getCmd)
}

func runGetCommand(command string, debug, alarm bool, latencyThreshold float64, window string) error {
	cfg, err := config.LoadConfig(configFilePath())
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	p, err := config.Prometheus(cfg)
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	client := prometheus.NewClient()
	auth := prometheus.NewAuth(p.Username, p.Password)
	baseURL := p.QueryURL()

	if debug {
		fmt.Fprintf(os.Stderr, "Prometheus: %s\n", p.URL)
	}

	clusterID := p.ClusterID
	selector := prometheus.BuildSelector([2]string{p.LabelName(), clusterID})

	var query string
	switch command {
	case "osd-latency":
		query = fmt.Sprintf("topk(5, sort_desc(ceph_osd_apply_latency_ms%s + ceph_osd_commit_latency_ms%s))", selector, selector)
	case "slow-ops":
		query = fmt.Sprintf("ceph_healthcheck_slow_ops%s", selector)
	case "bluestore-slow-committed-kv-count":
		query = fmt.Sprintf("topk(5, increase(ceph_bluestore_slow_committed_kv_count%s[%s]))", selector, window)
	}

	if query == "" {
		return nil
	}

	results, err := client.Query(baseURL, query, auth)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "Query error: %v\n", err)
		}
		return fmt.Errorf("query failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No data.")
		return nil
	}

	triggered := false
	for _, r := range results {
		value := prometheus.ExtractValue(r)
		osd := prometheus.FirstLabel(r.Metric, []string{"ceph_daemon", "osd", "instance"})
		if osd == "" {
			osd = "unknown"
		}

		switch command {
		case "slow-ops":
			fmt.Printf("%.0f slow ops\n", value)
			if value > 0 {
				triggered = true
			}
		case "bluestore-slow-committed-kv-count":
			fmt.Printf("  %s: %.0f slow KV commits\n", osd, value)
		case "osd-latency":
			fmt.Printf("  %s: %.2f ms\n", osd, value)
			if value > latencyThreshold {
				triggered = true
			}
		default:
			fmt.Printf("  %s: %.2f\n", osd, value)
		}
	}

	if alarm && triggered {
		os.Exit(3)
	}
	return nil
}

// runDropCommand handles osd/pool/rgw-performance-drop. It delegates to the
// same operator.Engine methods a rule's osd_performance_drop/
// pool_performance_drop/rgw_performance_drop condition uses, instead of
// re-implementing the drop-detection Prometheus queries a third time here.
func runDropCommand(kind string, threshold float64) error {
	cfg, err := config.LoadConfig(configFilePath())
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	eng := &operator.Engine{PromClient: prometheus.NewClient(), Config: cfg}
	kwargs := map[string]interface{}{}
	cond := &operator.RuleCondition{Threshold: &threshold}

	var result *operator.EvalResult
	switch kind {
	case "osd":
		result = eng.EvalOSDPerformanceDrop(kwargs, cond)
	case "pool":
		result = eng.EvalPoolPerformanceDrop(kwargs, cond)
	case "rgw":
		result = eng.EvalRGWPerformanceDrop(kwargs, cond)
	default:
		return fmt.Errorf("unknown drop type: %s", kind)
	}

	for _, line := range result.Logs {
		fmt.Println(line)
	}
	return nil
}
