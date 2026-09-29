package cli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/operator"
)

// sampleRule is written when the rules directory is bootstrapped; it must
// stay byte-identical to etc/ceph-companion/rules/stop_sluggish_osd_service.yaml
// (enforced by TestSampleRuleSync).
//
//go:embed sample_rule.yaml
var sampleRule string

var (
	opRulesDir  string
	opLive      bool
	opRule      string
	opListRules bool
	opDebug     bool
)

func init() {
	opCmd := &cobra.Command{
		Use:   "operator",
		Short: "Ceph Operator - automated remediation based on rules",
		Long: `Automated Ceph cluster remediation based on Prometheus metrics.
DRY-RUN by default. Use --yes-i-really-mean-it for live execution.`,
		RunE: runOperator,
	}

	opCmd.Flags().StringVar(&opRulesDir, "rules-dir", filepath.Join(config.SystemDir, "rules"), "Rules directory")
	opCmd.Flags().BoolVar(&opLive, "yes-i-really-mean-it", false, "Execute actions (default: dry-run)")
	opCmd.Flags().StringVar(&opRule, "rule", "", "Run specific rule")
	opCmd.Flags().BoolVar(&opListRules, "list-rules", false, "List available rules")
	opCmd.Flags().BoolVar(&opDebug, "debug", false, "Enable debug output")

	rootCmd.AddCommand(opCmd)
}

func runOperator(cmd *cobra.Command, args []string) error {
	// Explicit --rules-dir is strict: a missing directory is an error, the
	// run must not execute rules the operator never asked for. The default
	// chains ./rules -> binary dir -> system path and bootstraps a sample.
	rulesDir := opRulesDir
	if cmd.Flags().Changed("rules-dir") {
		if info, err := os.Stat(rulesDir); err != nil || !info.IsDir() {
			return fmt.Errorf("rules directory not found: %s", rulesDir)
		}
	} else {
		for _, cand := range []string{
			"rules",
			filepath.Join(filepath.Dir(os.Args[0]), "rules"),
			filepath.Join(config.SystemDir, "rules"),
		} {
			if info, err := os.Stat(cand); err == nil && info.IsDir() {
				rulesDir = cand
				break
			}
		}
		if info, err := os.Stat(rulesDir); err != nil || !info.IsDir() {
			if err := os.MkdirAll(rulesDir, 0o755); err != nil {
				return fmt.Errorf("rules directory not found: %w", err)
			}
			if err := os.WriteFile(filepath.Join(rulesDir, "sample.yaml"), []byte(sampleRule), 0o644); err != nil {
				return fmt.Errorf("failed to create sample rule: %w", err)
			}
			fmt.Println("Created sample rules directory:", rulesDir)
		}
	}

	eng := operator.NewEngine(rulesDir, opLive, configFilePath(), opDebug)

	if opListRules {
		eng.ListRules()
		return nil
	}

	return eng.Run(opRule)
}
