package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/version"
)

var configFlag string

var rootCmd = &cobra.Command{
	Use:     "ceph-companion",
	Short:   "Ceph infrastructure automation tool",
	Version: version.String(),
	Long: `A unified tool for Ceph cluster monitoring, analysis, and automation.
Combines Prometheus metrics fetching, OSD deep analysis, and rule-based remediation.`,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configFlag, "config", filepath.Join(config.SystemDir, "config.yaml"), "Path to the configuration file")
}

// configFilePath resolves the configuration file path: --config when
// explicitly set, otherwise $CEPH_COMPANION_CONFIG, otherwise the default
// shown in --help. The resolved path is used strictly - there is no
// ./config.yaml auto-detection.
func configFilePath() string {
	return resolveConfigPath(rootCmd.PersistentFlags().Changed("config"), configFlag, os.Getenv("CEPH_COMPANION_CONFIG"))
}

// resolveConfigPath: explicit flag beats CEPH_COMPANION_CONFIG beats the default.
func resolveConfigPath(explicit bool, flagValue, envValue string) string {
	if explicit {
		return flagValue
	}
	if envValue != "" {
		return envValue
	}
	return flagValue
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
