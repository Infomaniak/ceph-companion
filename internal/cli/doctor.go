package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/infomaniak/ceph-companion/internal/colors"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/operator"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
	"github.com/infomaniak/ceph-companion/internal/version"
)

var doctorWebhook bool

func init() {
	doctorCmd := &cobra.Command{
		Use:   "doctor",
		Short: "Sanity-check the environment: exec commands in PATH, Prometheus endpoint and exporter",
		Long: `Runs sanity checks for the environment ceph-companion runs in:

- paths:      external commands used in exec contexts are available in PATH
- prometheus: the configured endpoint is reachable and the ceph exporter
              scrapes this host (ceph_bluestore_slow_committed_kv_count
              with instance == hostname)
- webhook:    sends a test action message and a test error message to the
              configured notification webhook(s) (opt-in via --webhook,
              it posts to your chat channel)`,
		RunE:          runDoctor,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	doctorCmd.Flags().BoolVar(&doctorWebhook, "webhook", false, "Also send test action and error messages to the notification webhook(s)")
	rootCmd.AddCommand(doctorCmd)
}

type checkResult struct {
	Check  string
	OK     bool
	Detail string
}

// execBinary describes an external command ceph-companion shells out to.
type execBinary struct {
	Name   string
	UsedBy string
	Hint   string
}

var execBinaries = []execBinary{
	{"cephadm", "all ceph/cephadm CLI wrappers (internal/ceph)", "install: cephadm package"},
	{"journalctl", "kernel-log scans (kernel_disk_errors, SmartPQI correlation)", "provided by systemd"},
	{"lsblk", "device identity lookups (serial/WWN)", "provided by util-linux"},
	{"smartctl", "SMART media-error checks (smart_media_errors condition)", "install: smartmontools"},
	{"sh", "rule actions (ceph_cli command type)", "provided by any POSIX system"},
}

func runDoctor(cmd *cobra.Command, args []string) error {
	failures := 0
	report := func(results []checkResult) {
		for _, r := range results {
			status := fmt.Sprintf("%sOK%s     ", colors.Green, colors.Reset)
			if !r.OK {
				status = fmt.Sprintf("%sFAIL%s   ", colors.Red, colors.Reset)
				failures++
			}
			fmt.Printf("  %s %s\n", status, r.Detail)
		}
	}

	// The hostname is taken as-is from the server (our hosts should have
	// short hostnames); printing it verbatim makes any mismatch with the
	// exporter's instance label obvious in the checks below.
	hostname, hostErr := os.Hostname()
	if hostErr != nil || hostname == "" {
		hostname = "<unknown>"
	}
	cfg, cfgErr := configLoad()
	p, promErr := config.Prometheus(cfg)

	clusterID := "<unavailable>"
	if promErr == nil {
		clusterID = p.ClusterID
	}
	fmt.Printf("version: %s\n", version.String())
	fmt.Printf("config: %s\n", configFilePath())
	fmt.Printf("hostname: %s\n", hostname)
	fmt.Printf("cluster_id: %s\n", clusterID)

	fmt.Println("\npaths:")
	report(checkCommands(execBinaries))

	fmt.Println("\nprometheus:")
	if promErr != nil {
		reason := promErr
		if cfgErr != nil {
			reason = cfgErr
		}
		report([]checkResult{{Check: "config", Detail: fmt.Sprintf("configuration unusable: %v", reason)}})
	} else {
		report(promChecks(p, prometheus.NewClient(), hostname))
	}

	if doctorWebhook {
		fmt.Println("\nwebhook:")
		report(webhookChecks(cfg, hostname))
	}

	if failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	fmt.Printf("\n%sAll checks passed.%s\n", colors.Green, colors.Reset)
	return nil
}

// configLoad wraps config.LoadConfig with the resolved --config path.
func configLoad() (*config.Config, error) {
	return config.LoadConfig(configFilePath())
}

func checkCommands(bins []execBinary) []checkResult {
	results := make([]checkResult, 0, len(bins))
	for _, b := range bins {
		path, err := exec.LookPath(b.Name)
		if err != nil {
			results = append(results, checkResult{Check: b.Name, Detail: fmt.Sprintf("%-10s not in PATH — used by: %s (%s)", b.Name, b.UsedBy, b.Hint)})
			continue
		}
		results = append(results, checkResult{Check: b.Name, OK: true, Detail: fmt.Sprintf("%-10s %s — used by: %s", b.Name, path, b.UsedBy)})
	}
	return results
}

// promChecks verifies the endpoint is alive, that the exporter scrapes
// this host (an 'up' series per job for instance == hostname), and that
// the bluestore slow-KV metric has series for this host.
func promChecks(p *config.PrometheusConfig, client *prometheus.Client, hostname string) []checkResult {
	auth := prometheus.NewAuth(p.Username, p.Password)
	baseURL := p.QueryURL()
	clusterSel := prometheus.BuildSelector([2]string{p.LabelName(), p.ClusterID})
	results := make([]checkResult, 0, 3)

	if _, err := client.Query(baseURL, "up", auth); err != nil {
		return append(results, checkResult{Check: "alive", Detail: fmt.Sprintf("endpoint unreachable: %v (Prometheus: %s)", err, p.URL)})
	}
	results = append(results, checkResult{Check: "alive", OK: true, Detail: fmt.Sprintf("endpoint alive: %s", p.URL)})

	upSelector := prometheus.BuildSelector([2]string{p.LabelName(), p.ClusterID}, [2]string{"instance", hostname})
	upSeries, err := client.Query(baseURL, fmt.Sprintf("up%s", upSelector), auth)
	if err != nil {
		results = append(results, checkResult{Check: "scrape", Detail: fmt.Sprintf("scrape query failed: %v", err)})
		return results
	}
	if len(upSeries) == 0 {
		results = append(results, checkResult{Check: "scrape", Detail: fmt.Sprintf("no 'up' series for instance '%s' (cluster '%s') - check cluster_id/cluster_label or this host's exporter scrape", hostname, p.ClusterID)})
		return results
	}
	var jobs []string
	for _, s := range upSeries {
		job := prometheus.FirstLabel(s.Metric, []string{"job"})
		if job == "" {
			job = "<none>"
		}
		jobs = append(jobs, fmt.Sprintf("job=%s up=%.0f", job, prometheus.ExtractValue(s)))
	}
	results = append(results, checkResult{Check: "scrape", OK: true, Detail: fmt.Sprintf("exporter scrape for instance '%s': %s", hostname, strings.Join(jobs, ", "))})

	// A zero host count is diagnosed against the cluster-wide count to
	// tell "this host isn't scraped" from "metric missing entirely".
	hostSel := prometheus.BuildSelector([2]string{p.LabelName(), p.ClusterID}, [2]string{"instance", hostname})
	hostCount, herr := scalarCount(client, baseURL, fmt.Sprintf("count(ceph_bluestore_slow_committed_kv_count%s)", hostSel), auth)
	clusterCount, cerr := scalarCount(client, baseURL, fmt.Sprintf("count(ceph_bluestore_slow_committed_kv_count%s)", clusterSel), auth)
	switch {
	case herr != nil || cerr != nil:
		results = append(results, checkResult{Check: "metric", Detail: fmt.Sprintf("metric count query failed: %v", firstErr(herr, cerr))})
	case hostCount > 0:
		results = append(results, checkResult{Check: "metric", OK: true, Detail: fmt.Sprintf("ceph_bluestore_slow_committed_kv_count: %d series for instance '%s'", hostCount, hostname)})
	case clusterCount > 0:
		results = append(results, checkResult{Check: "metric", Detail: fmt.Sprintf("ceph_bluestore_slow_committed_kv_count: %d series cluster-wide but none for instance '%s' - hostname mismatch or this host's exporter is not scraped", clusterCount, hostname)})
	default:
		results = append(results, checkResult{Check: "metric", Detail: "ceph_bluestore_slow_committed_kv_count: missing cluster-wide - check the ceph exporter job and metric name"})
	}
	return results
}

// scalarCount extracts the single numeric value of an aggregate query
// (0 when the result is empty, which is how Prometheus reports count()
// over zero series).
func scalarCount(client *prometheus.Client, baseURL, query string, auth *prometheus.Auth) (int, error) {
	res, err := client.Query(baseURL, query, auth)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, nil
	}
	return int(prometheus.ExtractValue(res[0])), nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// webhookChecks sends a labeled action test message through the regular
// notification target and a labeled error test message through SendError
// (the dedicated error target when configured, the regular webhook with
// error_channel routing otherwise).
func webhookChecks(cfg *config.Config, hostname string) []checkResult {
	if cfg == nil || cfg.Notifications == nil || cfg.Notifications.Webhook == "" {
		return []checkResult{{Check: "webhook", Detail: "notifications not configured (no webhook in config.yaml)"}}
	}
	client := operator.NewNotificationClient(cfg.Notifications)
	actionMessage := fmt.Sprintf("ceph-companion doctor (%s): action test from %s", version.String(), hostname)
	errorMessage := fmt.Sprintf("ceph-companion doctor (%s): error test from %s", version.String(), hostname)

	results := make([]checkResult, 0, 2)
	if err := client.Send(actionMessage); err != nil {
		results = append(results, checkResult{Check: "webhook", Detail: fmt.Sprintf("action message to '%s' failed: %v", cfg.Notifications.Channel, err)})
	} else {
		results = append(results, checkResult{Check: "webhook", OK: true, Detail: fmt.Sprintf("action message sent to channel '%s'", cfg.Notifications.Channel)})
	}

	// Mirror SendError's per-field resolution for the report: error_channel
	// if set (else channel), error_webhook if set (else the regular webhook).
	errChannel := cfg.Notifications.ErrorChannel
	if errChannel == "" {
		errChannel = cfg.Notifications.Channel
	}
	fallbackNote := ""
	if cfg.Notifications.ErrorWebhook == "" {
		fallbackNote = " (error_webhook not set - fell back to the regular webhook)"
	}
	if err := client.SendError(errorMessage); err != nil {
		results = append(results, checkResult{Check: "error-webhook", Detail: fmt.Sprintf("error message to channel '%s' failed: %v%s", errChannel, err, fallbackNote)})
	} else {
		results = append(results, checkResult{Check: "error-webhook", OK: true, Detail: fmt.Sprintf("error message sent to channel '%s'%s", errChannel, fallbackNote)})
	}
	return results
}
