package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/infomaniak/ceph-companion/internal/ceph"
	"github.com/infomaniak/ceph-companion/internal/colors"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/osdanalyzer"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

var (
	osdID          string
	mockData       bool
	patternFile    string
	patternDir     string
	patternTest    string
	overrideSource string
	noFindings     bool
)

func init() {
	analyzeCmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze Ceph OSD performance using YAML pattern definitions",
		Long: `Deep OSD-level analysis using customizable YAML test definitions.
Supports historic operations, in-flight operations, and performance counter analysis.`,
		RunE: runAnalyze,
	}

	analyzeCmd.Flags().StringVar(&osdID, "osd-id", "0", "Target OSD ID or 'all' for all OSDs")
	analyzeCmd.Flags().BoolVar(&mockData, "mock-data", false, "Use built-in demo data instead of querying the live cluster (no cluster access needed)")
	analyzeCmd.Flags().StringVar(&patternFile, "file", "", "Single test definition file")
	analyzeCmd.Flags().StringVar(&patternDir, "patterns-dir", filepath.Join(config.SystemDir, "patterns"), "Directory containing YAML patterns")
	analyzeCmd.Flags().StringVar(&patternTest, "test", "", "Comma-separated list of pattern names from default dir")
	analyzeCmd.Flags().StringVar(&overrideSource, "source", "", "Override data source (osd_historic_ops, osd_perf_dump, osd_ops_in_flight, osd_counter_dump, prometheus)")
	analyzeCmd.Flags().BoolVar(&noFindings, "no-findings", false, "Suppress findings output")

	rootCmd.AddCommand(analyzeCmd)
}

func runAnalyze(cmd *cobra.Command, args []string) error {
	a := osdanalyzer.NewAnalyzer()
	if err := wireCluster(a); err != nil {
		return err
	}

	var osdIDs []int
	if strings.ToLower(osdID) == "all" {
		client := ceph.NewClient()
		ids, err := client.OSDLs()
		if err != nil {
			return fmt.Errorf("error getting OSD list: %w", err)
		}
		osdIDs = ids
		fmt.Printf("Running on all OSDs: %v\n", osdIDs)
	} else {
		id, err := strconv.Atoi(osdID)
		if err != nil {
			return fmt.Errorf("error: --osd-id must be an integer or 'all'")
		}
		osdIDs = []int{id}
	}

	var dryRunData map[string]interface{}
	if mockData {
		dryRunData = getMockData()
	}

	// Same contract as the operator's rules-dir: explicit = strict, default = chain.
	explicitPatterns := cmd.Flags().Changed("patterns-dir")

	for _, id := range osdIDs {
		fmt.Printf("\n%s%s%s\n", colors.Bold, strings.Repeat("=", 60), colors.Reset)
		fmt.Printf("%sRunning tests on OSD.%d%s\n", colors.Bold, id, colors.Reset)
		fmt.Printf("%s%s%s\n", colors.Bold, strings.Repeat("=", 60), colors.Reset)

		switch {
		case explicitPatterns:
			if info, err := os.Stat(patternDir); err != nil || !info.IsDir() {
				return fmt.Errorf("patterns directory not found: %s", patternDir)
			}
			a.RunTestsFromDirectory(patternDir, id, dryRunData, overrideSource, noFindings)
		case patternFile != "":
			pattern, err := osdanalyzer.LoadPattern(patternFile)
			if err != nil {
				return fmt.Errorf("error loading pattern: %w", err)
			}
			a.RunPattern(pattern, id, dryRunData, overrideSource, noFindings)
		case patternTest != "":
			patternsDir := resolvePatternsDir()
			tests := strings.Split(patternTest, ",")
			for _, test := range tests {
				name := strings.TrimSpace(test)
				testFile := filepath.Join(patternsDir, name+".yaml")
				if _, err := os.Stat(testFile); os.IsNotExist(err) {
					fmt.Printf("Test '%s' not found: %s\n", name, testFile)
					if rulePath := findRuleFile(name); rulePath != "" {
						fmt.Printf("Hint: '%s' is an operator rule (%s), not an analyze pattern.\nRun it with: ceph-companion operator --rule %s\n", name, rulePath, name)
					}
					continue
				}
				pattern, err := osdanalyzer.LoadPattern(testFile)
				if err != nil {
					fmt.Printf("Error loading %s: %v\n", testFile, err)
					continue
				}
				a.RunPattern(pattern, id, dryRunData, overrideSource, noFindings)
			}
		default:
			a.RunTestsFromDirectory(resolvePatternsDir(), id, dryRunData, overrideSource, noFindings)
		}
	}

	return nil
}

// wireCluster connects the analyzer to the configured Prometheus endpoint so
// patterns with `source: prometheus` can resolve metrics. Config problems are
// not an error here: ceph-sourced patterns work without any config, and
// prometheus-sourced patterns report the missing configuration themselves.
func wireCluster(a *osdanalyzer.Analyzer) error {
	cfg, cfgErr := config.LoadConfig(configFilePath())
	if cfgErr != nil {
		return nil
	}
	p, err := config.Prometheus(cfg)
	if err != nil {
		return nil
	}
	a.SetPrometheus(p, prometheus.NewClient())
	return nil
}

// resolvePatternsDir returns the first of ./patterns or SystemDir/patterns
// that exists, else ./patterns (so the existing "no patterns found" message
// applies). Only used when --patterns-dir was not explicitly set.
func resolvePatternsDir() string {
	if dirExists("patterns") {
		return "patterns"
	}
	if sysDir := filepath.Join(config.SystemDir, "patterns"); dirExists(sysDir) {
		return sysDir
	}
	return "patterns"
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// findRuleFile looks for <name>.yaml in ./rules or SystemDir/rules, ""
// if none - used to hint when an analyze --test name is an operator rule.
func findRuleFile(name string) string {
	for _, p := range []string{
		filepath.Join("rules", name+".yaml"),
		filepath.Join(config.SystemDir, "rules", name+".yaml"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func getMockData() map[string]interface{} {
	return map[string]interface{}{
		// Values for prometheus-source patterns (resolved by plain token
		// lookup when --mock-data is set). Dotted tokens resolve through
		// nested maps.
		"ceph_bluestore_slow_committed_kv_count": 12.0,
		"bluestore": map[string]interface{}{
			"slow_committed_kv_count": 12.0,
			// Real counter-dump shape: latency counters are
			// {avgcount, sum, avgtime} maps; .avgtime resolves to the
			// literal key (or sum/avgcount on older daemons).
			"state_done_lat": map[string]interface{}{"avgcount": 100.0, "sum": 8.0, "avgtime": 0.08},
		},
		// Scrub bookkeeping in the real osd-subsystem shape (see
		// patterns/osd_scrubs.yaml); values in seconds.
		"osd": []interface{}{
			map[string]interface{}{
				"labels": map[string]interface{}{},
				"counters": map[string]interface{}{
					"num_scrubs_started_ec": 180.0,
					"successful_scrubs_ec":  163.0,
					"successful_scrubs_ec_elapsed": map[string]interface{}{
						"avgcount": 163.0,
						"sum":      1467000.0,
						"avgtime":  9000.0,
					},
					"num_scrubs_started_replicated": 4.0,
					"successful_scrubs_replicated":  4.0,
					"successful_scrubs_replicated_elapsed": map[string]interface{}{
						"avgcount": 4.0,
						"sum":      3000.0,
						"avgtime":  750.0,
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
		"ops": []interface{}{
			map[string]interface{}{
				"duration":    858.52,
				"type":        "pg_backfill",
				"description": "progress 8.c79s2 e 36861/36861 lb 8:9e369d44:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.2",
			},
			map[string]interface{}{
				"duration":    849.87,
				"type":        "pg_backfill",
				"description": "progress 8.c79s2 e 36861/36861 lb 8:9e3681d2:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.2",
			},
			map[string]interface{}{
				"duration":    837.35,
				"type":        "MOSDPGPush",
				"description": "8.c79s2 36861/35038 [PushOp(8:9e369d56:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.170773.3",
			},
			map[string]interface{}{
				"duration":    835.47,
				"type":        "MOSDPGPush",
				"description": "8.c79s2 36861/35038 [PushOp(8:9e368ed3:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.237680.2",
			},
			map[string]interface{}{
				"duration":    831.30,
				"type":        "MOSDPGPush",
				"description": "8.c79s2 36861/35038 [PushOp(8:9e3681e4:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.237788.1",
			},
			map[string]interface{}{
				"duration":    750.21,
				"type":        "MOSDOp",
				"description": "8.a31c6b6e clientid client.4764372 objectid",
			},
			map[string]interface{}{
				"duration":    680.45,
				"type":        "MOSDPGScan",
				"description": "8.c79s2 scan range 8:9e369d44:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.2",
			},
			map[string]interface{}{
				"duration":    620.18,
				"type":        "MOSDPGBackfill",
				"description": "8.c79s2 backfill range 8:9e369d44:::20311fef-3375-4b14-a015-2ed1b9c2f2b2.2",
			},
			map[string]interface{}{
				"duration":    580.92,
				"type":        "MOSDPGInfo",
				"description": "8.c79s2 sub operation replication",
			},
		},
	}
}
