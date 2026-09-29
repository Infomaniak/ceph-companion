package operator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infomaniak/ceph-companion/internal/ceph"
	"github.com/infomaniak/ceph-companion/internal/colors"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// Engine runs the rule-based operator.
type Engine struct {
	RulesDir      string
	LiveMode      bool
	ConfigPath    string
	Debug         bool
	PromClient    *prometheus.Client
	CephClient    *ceph.Client
	Notifications *NotificationClient
	Config        *config.Config
	// DeviceCachePath overrides the persistent device-history cache file
	// (default: DeviceCacheFile). Only used by tests.
	DeviceCachePath string
}

// promConfig returns the configured Prometheus endpoint settings, or an
// error when no config was loaded or the mandatory fields (url, cluster_id)
// are missing. Conditions that need the endpoint funnel through this so a
// bad config always fails with the same actionable message.
func (e *Engine) promConfig() (*config.PrometheusConfig, error) {
	return config.Prometheus(e.Config)
}

// NewEngine creates a new operator engine. configPath is the resolved
// configuration file path (see cli.configFilePath).
func NewEngine(rulesDir string, liveMode bool, configPath string, debug bool) *Engine {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%sWarning: %v - conditions needing Prometheus config will be skipped%s\n", colors.Yellow, err, colors.Reset)
	}

	var notifications *NotificationClient
	if cfg != nil && cfg.Notifications != nil && cfg.Notifications.Webhook != "" {
		notifications = NewNotificationClient(cfg.Notifications)
	}

	return &Engine{
		RulesDir:      rulesDir,
		LiveMode:      liveMode,
		ConfigPath:    configPath,
		Debug:         debug,
		PromClient:    prometheus.NewClient(),
		CephClient:    ceph.NewClient(),
		Notifications: notifications,
		Config:        cfg,
	}
}

// ListRules lists available rules.
func (e *Engine) ListRules() {
	files, err := os.ReadDir(e.RulesDir)
	if err != nil {
		fmt.Printf("%sError reading rules directory: %v%s\n", colors.Red, err, colors.Reset)
		return
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(e.RulesDir, file.Name())
		rule, err := e.loadRule(path)
		if err != nil {
			continue
		}

		status := fmt.Sprintf("%sdisabled%s", colors.Red, colors.Reset)
		if rule.Enabled {
			status = fmt.Sprintf("%senabled%s", colors.Green, colors.Reset)
		}

		name := rule.Name
		if name == "" {
			name = strings.TrimSuffix(file.Name(), ".yaml")
		}
		fmt.Printf("%s%-30s%s [%s]\n", colors.Green, name, colors.Reset, status)
		fmt.Printf("    %s\n", rule.Description)
		fmt.Printf("    Conditions: %d\n\n", len(rule.Conditions))
	}
}

// Run executes rules. It returns an error - without running anything - if
// any selected rule fails validation, e.g. a condition that has no sensible
// built-in threshold (see conditionsRequiringThreshold) but doesn't set one,
// or an unknown condition command.
func (e *Engine) Run(ruleName string) error {
	rules, err := e.loadRules(ruleName)
	if err != nil {
		e.notifyExecutionErrors([]string{fmt.Sprintf("cannot read rules directory %s: %v", e.RulesDir, err)})
		fmt.Printf("%sNo rules found.%s\n", colors.Red, colors.Reset)
		return nil
	}

	if len(rules) == 0 {
		filter := ""
		if ruleName != "" {
			filter = fmt.Sprintf(" (filter: --rule %s)", ruleName)
		}
		e.notifyExecutionErrors([]string{fmt.Sprintf("no rules found in %s%s", e.RulesDir, filter)})
		fmt.Printf("%sNo rules found.%s\n", colors.Red, colors.Reset)
		return nil
	}

	for _, rule := range rules {
		if err := validateRule(rule); err != nil {
			return err
		}
	}

	e.printBanner()

	evaluated := 0
	processed := 0
	failed := 0
	var execErrs []string
	for _, rule := range rules {
		e.printRuleHeader(rule)

		if !rule.Enabled {
			if e.Debug {
				fmt.Printf("  %sDisabled, skipping%s\n", colors.Red, colors.Reset)
			}
			continue
		}
		evaluated++

		items, errs := e.evalRule(rule)
		execErrs = append(execErrs, errs...)
		if items == nil {
			if e.Debug {
				fmt.Printf("%sConditions not met%s\n", colors.Green, colors.Reset)
			}
			continue
		}

		if e.Debug {
			fmt.Printf("  %sTriggered!%s %d items\n", colors.Red, colors.Reset, len(items))
		}

		if ok, actionErrs := e.execAction(rule, items); ok {
			processed++
		} else {
			failed++
			execErrs = append(execErrs, actionErrs...)
		}
	}

	e.printDone(evaluated, processed, failed)

	// One batched kChat message per run about everything that went wrong
	// (condition evaluation failures, failed actions, missing rules), sent
	// in dry-run and live mode alike - the blind spot these report exists
	// in both.
	if len(execErrs) > 0 {
		e.notifyExecutionErrors(execErrs)
	}
	return nil
}

// conditionsRequiringThreshold lists commands with no sensible built-in
// threshold, where the rule author must set one explicitly in the YAML.
// osd_performance_drop/pool_performance_drop/rgw_performance_drop gate an
// automated OSD-stop action on a percentage drop - a missing threshold has
// no safe default (0% would trigger on any noise), so it's a hard error
// instead of silently picking a number.
var conditionsRequiringThreshold = map[string]bool{
	"osd_performance_drop":  true,
	"pool_performance_drop": true,
	"rgw_performance_drop":  true,
	// A prometheus_query condition without a threshold silently compares
	// against 0, which can make the rule always or never fire depending on
	// the metric - the rule author must state the number explicitly.
	"prometheus_query": true,
}

// knownConditionCommands is the set of condition types runCondition knows
// how to evaluate. validateRule rejects anything else so a typo'd command
// fails loudly at startup instead of silently skipping the rule on every
// run.
var knownConditionCommands = map[string]bool{
	"osd_performance_drop":  true,
	"pool_performance_drop": true,
	"rgw_performance_drop":  true,
	"kernel_disk_errors":    true,
	"smart_media_errors":    true,
	"osd_ok_to_stop":        true,
	"prometheus_query":      true,
}

// validateRule checks a rule's conditions for required fields the YAML
// parser can't enforce on its own.
func validateRule(rule *Rule) error {
	for i, cond := range rule.Conditions {
		if !knownConditionCommands[cond.Command] {
			return fmt.Errorf("rule %q condition %d: unknown condition command %q (known commands: osd_performance_drop, pool_performance_drop, rgw_performance_drop, kernel_disk_errors, smart_media_errors, osd_ok_to_stop, prometheus_query)", rule.Name, i+1, cond.Command)
		}
		if conditionsRequiringThreshold[cond.Command] && cond.Threshold == nil {
			return fmt.Errorf("rule %q condition %d (%s): missing required \"threshold\" field", rule.Name, i+1, cond.Command)
		}
	}
	return nil
}

func (e *Engine) printBanner() {
	if !e.Debug {
		return
	}
	mode := fmt.Sprintf("%sDRY-RUN%s", colors.Yellow, colors.Reset)
	if e.LiveMode {
		mode = fmt.Sprintf("%sLIVE%s", colors.Green, colors.Reset)
	}
	fmt.Printf("\n%s%s%s\n", colors.Bold, strings.Repeat("=", 50), colors.Reset)
	fmt.Printf("Mode: %s  Rules: %s\n", mode, e.RulesDir)
	fmt.Printf("%s%s%s\n\n", colors.Bold, strings.Repeat("=", 50), colors.Reset)
}

func (e *Engine) printRuleHeader(rule *Rule) {
	if !e.Debug {
		return
	}
	fmt.Printf("\n%s%s%s\n", colors.Bold, strings.Repeat("-", 40), colors.Reset)
	fmt.Printf("Rule: %s%s%s\n", colors.Bold, rule.Name, colors.Reset)
	fmt.Printf("  %s\n", rule.Description)
	fmt.Printf("%s%s%s\n", colors.Bold, strings.Repeat("-", 40), colors.Reset)
}

func (e *Engine) printDone(evaluated, processed, failed int) {
	if e.Debug {
		fmt.Printf("\n%s%s%s\n", colors.Bold, strings.Repeat("=", 50), colors.Reset)
		fmt.Printf("%sDone. Evaluated %d rule(s), executed %d action(s), %d failed.%s\n", colors.Green, evaluated, processed, failed, colors.Reset)
	} else if processed > 0 {
		fmt.Printf("%sDone: %d action(s) executed.%s\n", colors.Green, processed, colors.Reset)
	} else if failed > 0 {
		fmt.Printf("%sDone: %d action(s) failed.%s\n", colors.Red, failed, colors.Reset)
	} else {
		fmt.Printf("%sDone: no action needed.%s\n", colors.Green, colors.Reset)
	}
}

func (e *Engine) loadRule(path string) (*Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rule Rule
	if err := yaml.Unmarshal(data, &rule); err != nil {
		return nil, err
	}
	if rule.Name == "" {
		rule.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
	}
	return &rule, nil
}

func (e *Engine) loadRules(ruleName string) ([]*Rule, error) {
	files, err := os.ReadDir(e.RulesDir)
	if err != nil {
		return nil, err
	}

	var rules []*Rule
	seen := make(map[string]string) // rule name -> file it was loaded from
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(e.RulesDir, file.Name())
		rule, err := e.loadRule(path)
		if err != nil {
			continue
		}
		if ruleName != "" && rule.Name != ruleName {
			continue
		}
		// Two files with the same rule name would each be evaluated - in
		// live mode that means the action (e.g. stopping an OSD) runs
		// twice. Keep the first, skip the rest, and say so.
		if orig, dup := seen[rule.Name]; dup {
			fmt.Printf("%sWarning: duplicate rule name %q in %s (already loaded from %s) - skipping the duplicate%s\n", colors.Yellow, rule.Name, path, orig, colors.Reset)
			continue
		}
		seen[rule.Name] = path
		rules = append(rules, rule)
	}
	return rules, nil
}

func (e *Engine) evalRule(rule *Rule) ([]Item, []string) {
	var items []Item
	var runErrs []string

	for _, cond := range rule.Conditions {
		kwargs := map[string]interface{}{
			"debug": e.Debug,
		}

		if len(items) > 0 {
			kwargs["input_items"] = items
		}

		result := e.runCondition(cond.Command, kwargs, &cond)
		if result == nil {
			// Unknown command - validateRule already rejects these before
			// anything runs, so this is purely defensive.
			return nil, runErrs
		}

		// Execution errors are printed on every run (dry-run and live,
		// debug or not): a condition that cannot evaluate must never look
		// like a healthy "no match".
		for _, line := range result.Errors {
			fmt.Printf("    %s%s%s\n", colors.Red, line, colors.Reset)
			runErrs = append(runErrs, fmt.Sprintf("%s / %s: %s", rule.Name, cond.Command, line))
		}
		// AlwaysLog lines (e.g. cache invalidations) are state-changing
		// evidence that must reach the journal on every run: an invalidation
		// often *prevents* the trigger, so waiting for Triggered would hide
		// exactly the event that matters.
		for _, line := range result.AlwaysLog {
			fmt.Printf("    %s\n", line)
		}
		// Regular condition logs print in debug mode, and in live mode
		// whenever the condition triggered, so a journalled run is
		// self-documenting (why did this OSD get stopped?).
		if e.Debug || result.Triggered {
			for _, line := range result.Logs {
				fmt.Printf("    %s\n", line)
			}
		}

		if !result.Triggered {
			return nil, runErrs
		}

		// A condition that returns no items of its own is a pure gate (e.g.
		// a cluster-wide check like performance drop): it must still pass,
		// but it doesn't identify which entities the rule acts on, so the
		// items found by earlier conditions carry through unchanged instead
		// of being wiped out.
		if len(result.Items) == 0 {
			continue
		}

		items = result.Items
		if cond.Threshold != nil {
			items = thresholdFilter(items, *cond.Threshold, cond.Operator)
		}
		if len(items) == 0 {
			return nil, runErrs
		}
	}

	if len(items) == 0 {
		return nil, runErrs
	}
	return items, runErrs
}

func (e *Engine) runCondition(command string, kwargs map[string]interface{}, cond *RuleCondition) *EvalResult {
	// Map command names to evaluation functions. Simple "one metric + one
	// threshold" checks (OSD latency, BlueStore slow-KV, slow ops) are handled
	// generically by prometheus_query instead of a dedicated case here - only
	// conditions with real logic beyond a single PromQL comparison get one.
	switch command {
	case "osd_performance_drop":
		return e.EvalOSDPerformanceDrop(kwargs, cond)
	case "pool_performance_drop":
		return e.EvalPoolPerformanceDrop(kwargs, cond)
	case "rgw_performance_drop":
		return e.EvalRGWPerformanceDrop(kwargs, cond)
	case "kernel_disk_errors":
		return e.evalKernelLogs(kwargs, cond)
	case "smart_media_errors":
		return e.evalSmartMediaErrors(kwargs, cond)
	case "osd_ok_to_stop":
		return e.evalOSDOkToStop(kwargs, cond)
	case "prometheus_query":
		return e.evalPrometheusQuery(kwargs, cond)
	default:
		return nil
	}
}

// compareValue evaluates value <operator> threshold. Unknown or empty
// operators default to ">", matching the engine's historical behavior.
func compareValue(value float64, operator string, threshold float64) bool {
	switch operator {
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	case "!=":
		return value != threshold
	default:
		return value > threshold
	}
}

func thresholdFilter(items []Item, threshold float64, operator string) []Item {
	var filtered []Item
	for _, it := range items {
		if compareValue(it.Value, operator, threshold) {
			filtered = append(filtered, it)
		}
	}
	return filtered
}

func (e *Engine) execAction(rule *Rule, items []Item) (bool, []string) {
	action := rule.Action
	cmdTemplate := action.Command
	dryMsg := action.DryRunText
	if dryMsg == "" {
		dryMsg = fmt.Sprintf("WOULD EXECUTE: %s", cmdTemplate)
	}

	maxItems := action.MaxItemsPerRun
	if maxItems == 0 {
		maxItems = 1
	}
	if len(items) > maxItems {
		items = items[:maxItems]
	}

	if e.Debug {
		cmdRunes := []rune(cmdTemplate)
		fmt.Printf("  Action: %s%s...%s\n", colors.Cyan, string(cmdRunes[:min(60, len(cmdRunes))]), colors.Reset)
	}

	ok := true
	var errs []string
	for _, it := range items {
		cmd := render(cmdTemplate, it)
		msg := render(dryMsg, it)

		if !e.LiveMode {
			if e.Debug {
				fmt.Printf("    %s[DRY]%s %s\n", colors.Yellow, colors.Reset, msg)
			}
			continue
		}

		// Live mode journals the action trail itself: the rendered command
		// plus its outcome. ShellCommand captures stdout/stderr in memory,
		// so without these lines the companion's own journal entry contains
		// nothing but the final "Done: N action(s) executed." summary.
		fmt.Printf("    %s[Exec]%s %s\n", colors.Cyan, colors.Reset, cmd)
		success, out, err := e.CephClient.ShellCommand(cmd)
		status := "OK"
		statusColor := colors.Green
		if !success {
			status = "FAIL"
			statusColor = colors.Red
		}
		// cephadm writes "Inferring fsid..." noise to stderr even on
		// success, so stdout stays preferred there. On failure stderr
		// carries the actual reason, but it is buried at the end behind
		// that same noise (every `cephadm shell` stage of an &&-chain
		// adds its own preamble) - strip the noise so the FAIL line
		// reaches the real error instead of truncating inside it.
		output := out
		if !success && err != "" {
			if filtered := filterCephadmStderr(err); filtered != "" {
				output = filtered
			}
		}
		if outRunes := []rune(output); len(outRunes) > 120 {
			output = string(outRunes[:120])
		}
		fmt.Printf("    %s%s%s: %s\n", statusColor, status, colors.Reset, output)
		if !success {
			ok = false
			errs = append(errs, fmt.Sprintf("%s / action: command failed on %s: %s", rule.Name, it.FullName, output))
		}
	}

	if ok && action.SendNotification && e.Notifications != nil {
		e.notify(rule, items)
	}
	return ok, errs
}

// notifyExecutionErrors sends one batched kChat message about the
// execution errors of this run (condition evaluation failures, failed
// actions, missing rules). Best-effort: send failures print to stdout and
// never change the run's outcome; without a notifications client the errors
// stay on stdout only (a broken config.yaml hides its own webhook, so that
// blind spot is inherent).
func (e *Engine) notifyExecutionErrors(errs []string) {
	if e.Notifications == nil {
		fmt.Printf("%sExecution errors occurred but notifications are not configured - errors only printed above%s\n", colors.Yellow, colors.Reset)
		return
	}
	hostname, _ := os.Hostname()
	mode := "dry-run"
	if e.LiveMode {
		mode = "live"
	}
	if err := e.Notifications.SendError(buildErrorNotification(hostname, mode, errs)); err != nil {
		fmt.Printf("%sFailed to send execution-error notification: %v%s\n", colors.Red, err, colors.Reset)
	} else if e.Debug {
		fmt.Printf("    %sExecution-error notification sent%s\n", colors.Green, colors.Reset)
	}
}

// buildErrorNotification assembles the single kChat message describing the
// run's execution errors.
func buildErrorNotification(hostname, mode string, errs []string) string {
	var b strings.Builder
	b.WriteString("**ceph-companion execution errors**\n")
	fmt.Fprintf(&b, "**Host:** %s\n", hostname)
	fmt.Fprintf(&b, "**Mode:** %s\n", mode)
	for _, line := range errs {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	return b.String()
}

// filterCephadmStderr drops cephadm's per-command stderr preamble
// ("Inferring fsid ...", "Inferring config ...", "Using ceph image ...",
// bare image digests) so that a failed &&-chain surfaces the actual error
// line, which always comes last.
func filterCephadmStderr(s string) string {
	var kept []string
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" ||
			strings.HasPrefix(t, "Inferring ") ||
			strings.HasPrefix(t, "Using ceph image ") ||
			strings.HasPrefix(t, "quay.io/") {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, "; ")
}

func (e *Engine) notify(rule *Rule, items []Item) {
	var names []string
	for _, it := range items {
		if it.FullName != "" {
			names = append(names, it.FullName)
		} else {
			names = append(names, fmt.Sprintf("osd.%s", it.ID))
		}
	}

	msg := rule.Action.NotificationMsg
	if msg != "" {
		msg = strings.ReplaceAll(msg, "{items}", strings.Join(names, ", "))
		msg = strings.ReplaceAll(msg, "{count}", fmt.Sprintf("%d", len(items)))
		hostname, _ := os.Hostname()
		msg = strings.ReplaceAll(msg, "{host}", hostname)
		msg = strings.ReplaceAll(msg, "{cluster}", hostname)
	} else {
		msg = fmt.Sprintf("**Ceph Operator**\nRule: %s\nItems: %s\nCount: %d",
			rule.Name, strings.Join(names, ", "), len(items))
	}

	err := e.Notifications.Send(msg)
	if e.Debug {
		if err == nil {
			fmt.Printf("    %sNotification sent%s\n", colors.Green, colors.Reset)
		} else {
			fmt.Printf("    %sNotification failed: %v%s\n", colors.Red, err, colors.Reset)
		}
	}
}

// render replaces {key} placeholders with item values.
func render(cmd string, item Item) string {
	replacements := map[string]string{
		"{entity_type}": item.Type,
		"{full_name}":   item.FullName,
		"{id}":          item.ID,
		"{name}":        item.FullName,
		"{osd_id}":      item.OsdID,
		"{value}":       fmt.Sprintf("%.2f", item.Value),
	}

	for placeholder, value := range replacements {
		cmd = strings.ReplaceAll(cmd, placeholder, value)
	}
	return cmd
}
