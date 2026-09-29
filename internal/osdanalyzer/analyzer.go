package osdanalyzer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/infomaniak/ceph-companion/internal/ceph"
	"github.com/infomaniak/ceph-companion/internal/colors"
	"github.com/infomaniak/ceph-companion/internal/config"
	"github.com/infomaniak/ceph-companion/internal/prometheus"
)

// Analyzer is the main OSD analysis engine.
type Analyzer struct {
	client *ceph.Client

	// Prometheus-backed source support (pattern YAML: `source: prometheus`),
	// enabled via SetPrometheus. When unset, prometheus-sourced patterns
	// report a clear configuration error instead of silently comparing
	// against 0.
	cfg  *config.PrometheusConfig
	prom *prometheus.Client

	// fetchCache caches ceph-source fetches per (source, OSD) so a directory
	// of 19 pattern files costs one `cephadm shell` invocation per source
	// instead of 19. Fetch errors are cached too (with a "reported" flag so
	// the full error prints once, not once per pattern file).
	fetchCache map[string]*fetchResult
	// promCache caches resolved Prometheus values per final query string.
	promCache map[string]promCacheEntry
}

// fetchResult is a cached ceph-source fetch: data, the error that produced
// the nil data (surfaced to the user once), and whether it was printed.
type fetchResult struct {
	data     map[string]interface{}
	err      error
	reported bool
}

// promCacheEntry is a resolved Prometheus token value; found=false covers
// both "no such series" and "query failed" and is cached to avoid re-querying.
type promCacheEntry struct {
	value float64
	found bool
}

// NewAnalyzer creates a new Analyzer.
func NewAnalyzer() *Analyzer {
	return &Analyzer{
		client:     ceph.NewClient(),
		fetchCache: make(map[string]*fetchResult),
		promCache:  make(map[string]promCacheEntry),
	}
}

// SetPrometheus enables the Prometheus-backed data source, scoping all
// metric queries to the configured cluster (label + mandatory cluster_id
// from config.yaml).
func (a *Analyzer) SetPrometheus(cfg *config.PrometheusConfig, prom *prometheus.Client) {
	a.cfg = cfg
	a.prom = prom
}

// LoadPattern loads a YAML pattern file.
func LoadPattern(path string) (*AnalyzerPattern, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pattern AnalyzerPattern
	if err := yaml.Unmarshal(data, &pattern); err != nil {
		return nil, err
	}
	return &pattern, nil
}

// RunPattern runs a single pattern against an OSD.
func (a *Analyzer) RunPattern(pattern *AnalyzerPattern, osdID int, dryRunData map[string]interface{}, overrideSource string, noFindings bool) []EvalPatternResult {
	if len(pattern.Patterns) == 0 {
		fmt.Printf("Warning: pattern %q defines no patterns to evaluate (is this a rule file? rules belong in rules/ and run via 'ceph-companion operator --rule <name>')\n", pattern.Name)
		return nil
	}

	source := pattern.Source
	if overrideSource != "" {
		source = overrideSource
	}

	data := dryRunData
	if dryRunData == nil {
		switch source {
		case "prometheus":
			if a.cfg == nil || a.prom == nil {
				fmt.Println(colors.Wrap("source prometheus requires a configured endpoint (config.yaml with prometheus.url)", colors.Red))
				return nil
			}
			// No bulk fetch: each metric token in the pattern is resolved
			// against Prometheus during evaluation (see promResolver).
		default:
			res := a.cachedFetch(source, osdID)
			if res.err != nil {
				if res.reported {
					fmt.Println(colors.Wrap(fmt.Sprintf("No data available from source %s for OSD %d (fetch failed, see error above)", source, osdID), colors.Yellow))
				} else {
					res.reported = true
					fmt.Println(colors.Wrap(fmt.Sprintf("Failed to fetch %s for OSD %d: %v", source, osdID, res.err), colors.Red))
				}
				return nil
			}
			data = res.data
		}
	}

	if data == nil && source != "prometheus" {
		fmt.Println(colors.Wrap(fmt.Sprintf("No data available from source %s for OSD %d", source, osdID), colors.Yellow))
		return nil
	}

	var results []EvalPatternResult
	for _, p := range pattern.Patterns {
		var result EvalPatternResult
		if source == "prometheus" && dryRunData == nil {
			// Live Prometheus: resolve each metric token via the cluster's
			// endpoint. With --mock-data, fall through to the default
			// deepGet resolver so mock maps can provide ceph_* keys.
			result = EvaluatePatternWithResolver(p, data, source, a.promResolver(osdID))
		} else {
			result = EvaluatePattern(p, data, source)
		}
		results = append(results, result)

		statusText := colors.Wrap("✅ False", colors.Green)
		if result.Success {
			statusText = colors.Wrap("🔴 True", colors.Red)
		}
		fmt.Printf("[Pattern] %s → %s\n", p, statusText)
		fmt.Printf("         %s\n", result.Message)

		for key, value := range result.FoundValues {
			if key == "top_ops" {
				if ops, ok := value.([]map[string]interface{}); ok && len(ops) > 0 {
					fmt.Printf("         TOP %d SLOWEST OPERATIONS:\n", len(ops))
					fmt.Println("         " + strings.Repeat("-", 40))
					for i, op := range ops {
						formatted := FormatOperation(op)
						fmt.Printf("         %d. %s\n", i+1, formatted)
					}
				}
			} else {
				fmt.Printf("         Found %s = %v\n", key, value)
			}
		}
	}

	triggered := false
	for _, r := range results {
		if r.Success {
			triggered = true
			break
		}
	}

	if triggered && !noFindings {
		fmt.Println(colors.Wrap("\n        ↓ Matched Patterns Triggered Findings:", colors.Yellow))
		foundValues := make(map[string]interface{})
		for _, r := range results {
			for k, v := range r.FoundValues {
				foundValues[k] = v
			}
		}

		for _, finding := range pattern.Findings {
			expanded := finding
			for key, value := range foundValues {
				if key == "top_ops" {
					continue
				}
				placeholder := fmt.Sprintf("{%s}", key)
				expanded = strings.ReplaceAll(expanded, placeholder, fmt.Sprintf("%v", value))
			}
			fmt.Printf("[Finding] %s\n", expanded)

			if strings.Contains(finding, "top_ops") {
				if ops, ok := foundValues["top_ops"].([]map[string]interface{}); ok && len(ops) > 0 {
					fmt.Println("         Detailed slow operations:")
					for i, op := range ops {
						formatted := FormatOperation(op)
						fmt.Printf("         %d. %s\n", i+1, formatted)
					}
				}
			}
		}
	}

	return results
}

// TokenResolver resolves a metric token to a numeric value. The default
// resolver walks the fetched ceph JSON (deepGet); the analyzer supplies a
// Prometheus-backed resolver for `source: prometheus` patterns.
type TokenResolver func(token string) (float64, bool)

// defaultTokenResolver resolves tokens against the fetched ceph JSON.
func defaultTokenResolver(data map[string]interface{}) TokenResolver {
	return func(token string) (float64, bool) {
		return resolveCephToken(data, token)
	}
}

// resolveCephToken resolves one token against raw ceph daemon JSON (counter
// dump / perf dump). Counter values are either plain numbers (scalars) or
// maps like {"avgcount": N, "sum": S, "avgtime": T}. Semantics:
//
//   - "path.avgtime" -> literal "avgtime" when the daemon emits it (newer
//     Ceph) and avgcount > 0; falls back to computing sum/avgcount for older
//     daemons that only emit {avgcount, sum}. Unresolvable when avgcount == 0
//     (nothing has been averaged yet — mirrors "no such series" on the
//     prometheus source).
//   - "path"         -> scalars as-is; a counter map resolves to "sum" (the
//     cumulative total), so path-vs-path patterns work on map-typed counters.
//   - "path.avgcount" / "path.sum" resolve via the literal keys (deepGet).
func resolveCephToken(data map[string]interface{}, token string) (float64, bool) {
	if strings.HasSuffix(token, ".avgtime") {
		parent := deepGet(data, strings.TrimSuffix(token, ".avgtime"))
		m, ok := parent.(map[string]interface{})
		if !ok {
			return 0, false
		}
		avg, ok := toFloat64(m["avgcount"])
		if !ok || avg <= 0 {
			return 0, false
		}
		if t, ok := toFloat64(m["avgtime"]); ok {
			return t, true
		}
		sum, ok := toFloat64(m["sum"])
		if !ok {
			return 0, false
		}
		return sum / avg, true
	}

	v := deepGet(data, token)
	if m, ok := v.(map[string]interface{}); ok {
		if _, isCounter := m["avgcount"]; !isCounter {
			return 0, false
		}
		sum, ok := toFloat64(m["sum"])
		if !ok {
			return 0, false
		}
		return sum, true
	}
	return toFloat64(v)
}

// EvaluatePattern evaluates a single pattern against data.
func EvaluatePattern(pattern string, data map[string]interface{}, sourceType string) EvalPatternResult {
	return EvaluatePatternWithResolver(pattern, data, sourceType, defaultTokenResolver(data))
}

// EvaluatePatternWithResolver evaluates a single pattern, resolving metric
// tokens through the given resolver.
func EvaluatePatternWithResolver(pattern string, data map[string]interface{}, sourceType string, resolve TokenResolver) EvalPatternResult {
	if resolve == nil {
		resolve = defaultTokenResolver(data)
	}
	result := EvalPatternResult{
		Success:     false,
		Message:     "",
		FoundValues: make(FoundValues),
		Pattern:     pattern,
		SourceType:  sourceType,
	}

	// source: prometheus only supports plain metric comparisons. The
	// ceph-JSON-specific functions below have no Prometheus equivalent, so
	// reject them clearly instead of mis-evaluating (e.g. count(events)
	// would otherwise fall through to the generic evaluator and parse
	// garbage).
	if sourceType == "prometheus" {
		if unsupportedWithPrometheus(pattern) {
			result.Message = "pattern function (TOP/event_duration/count/list_*/scrub_blocked_by) is not supported with source prometheus — use a ceph source or an operator rule"
			return result
		}
		success, foundValues, message := evaluateGenericExpressionRegex(pattern, promTokenRe, resolve)
		result.Success = success
		result.Message = message
		for k, v := range foundValues {
			result.FoundValues[k] = v
		}
		return result
	}

	topMatchFilter := regexp.MustCompile(`(?i)TOP\s*\(\s*(\d+)\s*,\s*([a-zA-Z0-9_\-\.]+)\s*,\s*['"](.+?)['"]\s*\)`)
	topMatchSimple := regexp.MustCompile(`(?i)TOP\s*\(\s*(\d+)\s*,\s*([a-zA-Z0-9_\-\.]+)\s*\)`)

	if m := topMatchFilter.FindStringSubmatch(pattern); m != nil {
		count, _ := strconv.Atoi(m[1])
		dataSource := m[2]
		descriptionFilter := m[3]

		if dataSource == "ops" {
			topOps := getTopOperations(data, count, descriptionFilter)
			result.FoundValues["top_ops"] = topOps
			result.Success = len(topOps) > 0
			result.Message = fmt.Sprintf("Found %d ops matching filter", len(topOps))
			return result
		}
		value := deepGet(data, dataSource)
		result.FoundValues[dataSource] = value
		result.Success = value != nil
		result.Message = fmt.Sprintf("Found %s = %v", dataSource, value)
		return result
	}

	if m := topMatchSimple.FindStringSubmatch(pattern); m != nil {
		count, _ := strconv.Atoi(m[1])
		dataSource := m[2]

		if dataSource == "ops" {
			topOps := getTopOperations(data, count, "")
			result.FoundValues["top_ops"] = topOps
			result.Success = len(topOps) > 0
			result.Message = fmt.Sprintf("Found %d top ops", len(topOps))
			return result
		}
		value := deepGet(data, dataSource)
		result.FoundValues[dataSource] = value
		result.Success = value != nil
		result.Message = fmt.Sprintf("Found %s = %v", dataSource, value)
		return result
	}

	eventFilterRe := regexp.MustCompile(`event_duration\(['"]([^'"]+)['"],\s*['"](.+?)['"]\)`)
	eventSimpleRe := regexp.MustCompile(`event_duration\(['"]([^'"]+)['"]\)`)

	if m := eventFilterRe.FindStringSubmatch(pattern); m != nil {
		eventType := m[1]
		descriptionFilter := m[2]
		maxDur := getMaxEventDuration(data, eventType, descriptionFilter)

		key := fmt.Sprintf("event_duration('%s', '%s')", eventType, descriptionFilter)
		result.FoundValues[key] = maxDur
		result.Message = fmt.Sprintf("Max %s duration with filter '%s' = %.2f", eventType, descriptionFilter, maxDur)

		safeExpr := eventFilterRe.ReplaceAllString(pattern, fmt.Sprintf("%.2f", maxDur))
		result.Success = evaluateSimpleExpression(safeExpr)
		return result
	}

	if m := eventSimpleRe.FindStringSubmatch(pattern); m != nil {
		eventType := m[1]
		maxDur := getMaxEventDuration(data, eventType, "")

		key := fmt.Sprintf("event_duration('%s')", eventType)
		result.FoundValues[key] = maxDur
		result.Message = fmt.Sprintf("Max %s duration = %.2f", eventType, maxDur)

		safeExpr := eventSimpleRe.ReplaceAllString(pattern, fmt.Sprintf("%.2f", maxDur))
		result.Success = evaluateSimpleExpression(safeExpr)
		return result
	}

	countFilterRe := regexp.MustCompile(`(?i)count\s*\(\s*events\s*,\s*['"](.+?)['"]\s*\)`)
	countSimpleRe := regexp.MustCompile(`(?i)count\s*\(\s*events\s*\)`)

	if m := countFilterRe.FindStringSubmatch(pattern); m != nil {
		filter := m[1]
		count := countEvents(data, filter)
		result.FoundValues["count(events)"] = count
		result.Message = fmt.Sprintf("Count of events = %d", count)
		safeExpr := countFilterRe.ReplaceAllString(pattern, fmt.Sprintf("%d", count))
		result.Success = evaluateSimpleExpression(safeExpr)
		return result
	}

	if countSimpleRe.MatchString(pattern) {
		count := countEvents(data, "")
		result.FoundValues["count(events)"] = count
		result.Message = fmt.Sprintf("Count of events = %d", count)
		safeExpr := countSimpleRe.ReplaceAllString(pattern, fmt.Sprintf("%d", count))
		result.Success = evaluateSimpleExpression(safeExpr)
		return result
	}

	if regexp.MustCompile(`(?i)^list_events`).MatchString(pattern) {
		eventCounts, eventSubtypes := listEvents(data)
		result.FoundValues["list_events"] = eventCounts
		result.FoundValues["list_events_subtypes"] = eventSubtypes
		result.Message = "Event types, subtypes, and operation details counted"
		result.Success = true
		return result
	}

	if regexp.MustCompile(`(?i)^list_clients`).MatchString(pattern) {
		clients, clientIPs, _ := listClients(data)
		result.FoundValues["list_clients"] = clients
		result.FoundValues["client_ips"] = clientIPs
		result.Message = "Client information counted"
		result.Success = true
		return result
	}

	// Handle scrub_blocked_by(count): counts in-flight ops waiting for
	// scrub. The call evaluates to the number of ops waiting on the single
	// most-contended object, gated by the argument: it is 0 unless at least
	// that many ops wait on the same object, so "scrub_blocked_by(3) > 0"
	// fires only when 3+ ops pile up on one object.
	scrubBlockedRe := regexp.MustCompile(`(?i)scrub_blocked_by\s*\(\s*(\d+)\s*\)`)
	if m := scrubBlockedRe.FindStringSubmatch(pattern); m != nil {
		minPerObject, _ := strconv.Atoi(m[1])
		totalBlocked, topObject, maxBlocked, bucketUUID, bucketShard, bucketType, blockedObjects := scrubBlockedByStats(data)

		result.FoundValues["scrub_total_blocked"] = totalBlocked
		result.FoundValues["scrub_top_object"] = topObject
		result.FoundValues["scrub_top_count"] = maxBlocked
		result.FoundValues["scrub_bucket_uuid"] = bucketUUID
		result.FoundValues["scrub_bucket_shard"] = bucketShard
		result.FoundValues["scrub_bucket_type"] = bucketType
		result.FoundValues["scrub_blocked_objects"] = blockedObjects
		result.Message = fmt.Sprintf("%d ops blocked by scrub; '%s' has %d (type=%s, bucket/user=%s, shard=%s)",
			totalBlocked, topObject, maxBlocked, bucketType, bucketUUID, bucketShard)

		value := maxBlocked
		if maxBlocked < minPerObject {
			value = 0
			result.Message += fmt.Sprintf(" — below the per-object threshold of %d", minPerObject)
		}

		safeExpr := scrubBlockedRe.ReplaceAllString(pattern, strconv.Itoa(value))
		result.Success = evaluateSimpleExpression(safeExpr)
		return result
	}

	// scrub_blocked_by without a numeric argument (e.g. "scrub_blocked_by()"
	// or bare "scrub_blocked_by") would otherwise fall through to the generic
	// evaluator and silently evaluate to false — reject it clearly instead.
	if regexp.MustCompile(`(?i)scrub_blocked_by\s*(\(\s*\))?\s*(?:>|>=|<=|<|==|!=|$)`).MatchString(pattern) {
		result.Message = "scrub_blocked_by requires a numeric argument: scrub_blocked_by(n), n = minimum ops piling up on the same object"
		return result
	}

	// Generic metric evaluation - handles both "path > literal" and
	// "path > path" comparisons (and AND/OR combinations of either) by
	// resolving every metric-path-looking token in the pattern via deepGet
	// and substituting it before evaluating.
	success, foundValues, message := evaluateGenericExpressionRegex(pattern, dottedTokenRe, resolve)
	result.Success = success
	result.Message = message
	for k, v := range foundValues {
		result.FoundValues[k] = v
	}
	return result
}

// scrubBlockedByStats scans in-flight ops for ones blocked waiting on scrub,
// identifies the most-contended object, and classifies it as an RGW
// bucket-index or user-bucket object from its Ceph object-name encoding.
func scrubBlockedByStats(data map[string]interface{}) (totalBlocked int, topObject string, maxBlocked int, bucketUUID, bucketShard, bucketType string, blockedObjects map[string]int) {
	blockedObjects = make(map[string]int)
	var order []string

	if opsRaw, ok := data["ops"].([]interface{}); ok {
		for _, opRaw := range opsRaw {
			op, ok := opRaw.(map[string]interface{})
			if !ok {
				continue
			}
			typeData, _ := op["type_data"].(map[string]interface{})
			if typeData == nil || typeData["flag_point"] != "waiting for scrub" {
				continue
			}
			totalBlocked++

			desc, _ := op["description"].(string)
			obj := desc
			parts := strings.SplitN(desc, " ", 3)
			if len(parts) >= 3 {
				remainder := parts[2]
				if idx := strings.Index(remainder, "["); idx >= 0 {
					obj = strings.TrimSpace(remainder[:idx])
				} else {
					obj = strings.TrimSpace(remainder)
				}
			}
			if _, seen := blockedObjects[obj]; !seen {
				order = append(order, obj)
			}
			blockedObjects[obj]++
		}
	}

	topObject = "none"
	for _, obj := range order {
		if blockedObjects[obj] > maxBlocked {
			maxBlocked = blockedObjects[obj]
			topObject = obj
		}
	}

	bucketUUID, bucketShard, bucketType = "unknown", "unknown", "unknown"
	if strings.Contains(topObject, ".dir.") {
		bucketType = "bucket_index"
		dirRe := regexp.MustCompile(`\.dir\.([0-9a-f-]+(?:\.\d+)*\.\d+):head`)
		if m := dirRe.FindStringSubmatch(topObject); m != nil {
			bucketParts := strings.Split(m[1], ".")
			bucketUUID = bucketParts[0]
			bucketShard = bucketParts[len(bucketParts)-1]
		}
	} else if strings.Contains(topObject, "users.uid::") {
		bucketType = "user_buckets"
		uidRe := regexp.MustCompile(`users\.uid::([0-9a-fA-F]+)`)
		if m := uidRe.FindStringSubmatch(topObject); m != nil {
			bucketUUID = m[1]
		}
	}

	return totalBlocked, topObject, maxBlocked, bucketUUID, bucketShard, bucketType, blockedObjects
}

// dottedTokenRe matches metric-path-like tokens (e.g.
// "rocksdb.submit_latency.avgtime", "osd_scrub_dp_ec[0].counters.num_scrubs_started")
// appearing anywhere in a pattern expression, so both sides of a comparison
// can be resolved and substituted before evaluation.
var dottedTokenRe = regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_.:\[\]]*\.[a-zA-Z_][a-zA-Z0-9_.:\[\]]*`)

// promTokenRe matches the two token forms allowed in `source: prometheus`
// patterns: raw ceph exporter metric names (undotted, e.g.
// "ceph_bluestore_slow_committed_kv_count") and perf-dump-style dotted paths
// (e.g. "bluestore.state_done_lat.avgtime") which promQueryForToken maps to
// their exporter metric names.
var promTokenRe = regexp.MustCompile(`ceph_[a-zA-Z0-9_]+|[a-zA-Z][a-zA-Z0-9_-]*(?:\.[a-zA-Z0-9_]+)+`)

// promUnsupportedRes are the ceph-JSON-only pattern functions, rejected with
// a clear message under source: prometheus.
var promUnsupportedRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)TOP\s*\(`),
	regexp.MustCompile(`(?i)event_duration\s*\(`),
	regexp.MustCompile(`(?i)count\s*\(\s*events`),
	regexp.MustCompile(`(?i)^list_events`),
	regexp.MustCompile(`(?i)^list_clients`),
	regexp.MustCompile(`(?i)scrub_blocked_by\s*\(`),
}

func unsupportedWithPrometheus(pattern string) bool {
	for _, re := range promUnsupportedRes {
		if re.MatchString(pattern) {
			return true
		}
	}
	return false
}

// evaluateGenericExpressionRegex is the generic evaluator with an explicit
// token regex + resolver, shared by the ceph-JSON sources (dottedTokenRe +
// deepGet) and source: prometheus (promTokenRe + PromQL lookup). It
// tokenizes every metric-path-looking substring in pattern, resolves each
// via the resolver, substitutes its numeric value, and evaluates the
// resulting comparison via evaluateSimpleExpression - which already
// supports >,>=,<,<=,==,!=,AND,OR over numeric operands. This supports
// "path > literal" and "path > path" alike. If any token fails to resolve
// to a number, evaluation is skipped instead of silently comparing against
// zero.
func evaluateGenericExpressionRegex(pattern string, tokenRe *regexp.Regexp, resolve TokenResolver) (bool, map[string]interface{}, string) {
	foundValues := make(map[string]interface{})

	tokens := tokenRe.FindAllString(pattern, -1)
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]bool)
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	// Substitute longer tokens first so a token that's a prefix of another
	// (e.g. "a.b" vs "a.b.c") can't corrupt the longer one's text.
	sort.Slice(unique, func(i, j int) bool { return len(unique[i]) > len(unique[j]) })

	safeExpr := pattern
	missing := false
	for _, token := range unique {
		value, ok := resolve(token)
		if !ok {
			foundValues[token] = "no data found"
			missing = true
			continue
		}
		foundValues[token] = value
		safeExpr = strings.ReplaceAll(safeExpr, token, strconv.FormatFloat(value, 'f', -1, 64))
	}

	if missing {
		return false, foundValues, "Some metrics not found — skipping evaluation"
	}
	return evaluateSimpleExpression(safeExpr), foundValues, "Pattern evaluated successfully"
}

// cachedFetch: per-(source, OSD) fetch cache - see the fetchCache field.
func (a *Analyzer) cachedFetch(source string, osdID int) *fetchResult {
	if a.fetchCache == nil {
		a.fetchCache = make(map[string]*fetchResult)
	}
	key := fmt.Sprintf("%s:%d", source, osdID)
	if res, ok := a.fetchCache[key]; ok {
		return res
	}
	res := &fetchResult{}
	switch source {
	case "osd_historic_ops":
		res.data, res.err = a.client.OSDHistoricOps(osdID)
	case "osd_perf_dump":
		res.data, res.err = a.client.OSDPerfDump(osdID)
	case "osd_ops_in_flight":
		res.data, res.err = a.client.OSDOpsInFlight(osdID)
	case "osd_counter_dump":
		res.data, res.err = a.client.OSDCounterDump(osdID)
	default:
		res.err = fmt.Errorf("unknown data source %q (known: osd_historic_ops, osd_perf_dump, osd_ops_in_flight, osd_counter_dump, prometheus)", source)
	}
	a.fetchCache[key] = res
	return res
}

// promResolver returns a TokenResolver that resolves pattern tokens against
// the configured cluster's Prometheus, scoped to a single OSD.
func (a *Analyzer) promResolver(osdID int) TokenResolver {
	return func(token string) (float64, bool) {
		return a.promValue(osdID, token)
	}
}

// promValue resolves one token to its instant value for an OSD, caching per
// final query string. Tries the ceph_daemon="osd.N" label first and falls
// back to osd="N" (both label conventions exist across exporter metrics).
func (a *Analyzer) promValue(osdID int, token string) (float64, bool) {
	if a.cfg == nil || a.prom == nil {
		return 0, false
	}
	expr, ok := promQueryForToken(token)
	if !ok {
		return 0, false
	}
	for _, selector := range a.osdSelectors(osdID) {
		query := strings.ReplaceAll(expr, "{SEL}", selector)
		if entry, ok := a.promCache[query]; ok {
			return entry.value, entry.found
		}
		value, found := a.execQuery(query)
		a.promCache[query] = promCacheEntry{value: value, found: found}
		if found {
			return value, true
		}
	}
	return 0, false
}

// osdSelectors returns the label selectors tried, in order, to scope a query
// to one OSD: the daemon-level label used by perf-counter metrics
// (ceph_daemon="osd.N") first, then the plain osd label. Both carry the
// mandatory cluster scoping matcher.
func (a *Analyzer) osdSelectors(osdID int) []string {
	osdDaemon := fmt.Sprintf("osd.%d", osdID)
	primary := prometheus.BuildSelector(
		[2]string{"ceph_daemon", osdDaemon},
		[2]string{a.cfg.LabelName(), a.cfg.ClusterID},
	)
	fallback := prometheus.BuildSelector(
		[2]string{"osd", fmt.Sprintf("%d", osdID)},
		[2]string{a.cfg.LabelName(), a.cfg.ClusterID},
	)
	if fallback == primary {
		return []string{primary}
	}
	return []string{primary, fallback}
}

// execQuery runs one instant query. Zero series means not found; multiple
// series (per-shard counters) resolve to the max so thresholds see the
// worst-case value.
func (a *Analyzer) execQuery(query string) (float64, bool) {
	results, err := a.prom.Query(a.cfg.QueryURL(), query, prometheus.NewAuth(a.cfg.Username, a.cfg.Password))
	if err != nil {
		fmt.Println(colors.Wrap(fmt.Sprintf("Warning: prometheus query %q failed: %v", query, err), colors.Yellow))
		return 0, false
	}
	if len(results) == 0 {
		return 0, false
	}
	best := prometheus.ExtractValue(results[0])
	for _, r := range results[1:] {
		if v := prometheus.ExtractValue(r); v > best {
			best = v
		}
	}
	return best, true
}

// promQueryForToken maps a `source: prometheus` pattern token to a PromQL
// expression with {SEL} label-selector placeholders.
//
//   - raw exporter metric names ("ceph_bluestore_slow_committed_kv_count")
//     are queried as instant values,
//   - perf-dump-style paths map to the exporter's _sum/_count gauge pairs,
//     preserving perf dump's lifetime-average semantics:
//     "sub.counter.avgtime" -> "ceph_sub_counter_sum{SEL} / ceph_sub_counter_count{SEL}",
//     "sub.counter.avgcount" -> "..._count", "sub.counter.sum" -> "..._sum",
//     and a bare "sub.counter" (or ".value") -> the gauge itself. Dashes in
//     subsystem names are sanitized to underscores (PromQL).
func promQueryForToken(token string) (string, bool) {
	if strings.HasPrefix(token, "ceph_") {
		return token + "{SEL}", true
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", false
	}
	base := "ceph_" + strings.ReplaceAll(parts[0], "-", "_") + "_" + parts[1]
	if len(parts) == 2 {
		return base + "{SEL}", true
	}
	switch parts[2] {
	case "value":
		return base + "{SEL}", true
	case "avgtime":
		return base + "_sum{SEL} / " + base + "_count{SEL}", true
	case "sum":
		return base + "_sum{SEL}", true
	case "avgcount":
		return base + "_count{SEL}", true
	}
	return "", false
}

func getTopOperations(data map[string]interface{}, count int, descriptionFilter string) []map[string]interface{} {
	opsRaw, ok := data["ops"].([]interface{})
	if !ok {
		return nil
	}

	var ops []map[string]interface{}
	for _, opRaw := range opsRaw {
		op, ok := opRaw.(map[string]interface{})
		if !ok {
			continue
		}
		desc, _ := op["description"].(string)
		if descriptionFilter != "" && !strings.Contains(desc, descriptionFilter) {
			continue
		}
		ops = append(ops, op)
	}

	sort.Slice(ops, func(i, j int) bool {
		di, _ := toFloat64(ops[i]["duration"])
		dj, _ := toFloat64(ops[j]["duration"])
		return di > dj
	})

	if len(ops) > count {
		ops = ops[:count]
	}
	return ops
}

func getMaxEventDuration(data map[string]interface{}, eventType, descriptionFilter string) float64 {
	maxDur := 0.0
	opsRaw, ok := data["ops"].([]interface{})
	if !ok {
		return maxDur
	}

	for _, opRaw := range opsRaw {
		op, ok := opRaw.(map[string]interface{})
		if !ok {
			continue
		}

		if descriptionFilter != "" {
			desc, _ := op["description"].(string)
			if !strings.Contains(desc, descriptionFilter) {
				continue
			}
		}

		typeData, ok := op["type_data"].(map[string]interface{})
		if !ok {
			continue
		}

		eventsRaw, ok := typeData["events"].([]interface{})
		if !ok {
			continue
		}

		for _, evtRaw := range eventsRaw {
			evt, ok := evtRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if evt["event"] == eventType {
				dur, _ := toFloat64(evt["duration"])
				if dur > maxDur {
					maxDur = dur
				}
			}
		}
	}
	return maxDur
}

func countEvents(data map[string]interface{}, filter string) int {
	count := 0

	if inFlight, ok := data["in_flight"].([]interface{}); ok {
		for _, opRaw := range inFlight {
			op, ok := opRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if filter == "" {
				count++
				continue
			}
			desc, _ := op["description"].(string)
			if strings.Contains(desc, filter) {
				count++
			}
		}
		return count
	}

	if opsRaw, ok := data["ops"].([]interface{}); ok {
		for _, opRaw := range opsRaw {
			op, ok := opRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if filter == "" {
				count++
				continue
			}
			desc, _ := op["description"].(string)
			if strings.Contains(desc, filter) {
				count++
			}
		}
	}
	return count
}

func listEvents(data map[string]interface{}) (map[string]int, map[string]int) {
	eventCounts := make(map[string]int)
	eventSubtypes := make(map[string]int)

	opsRaw, ok := data["ops"].([]interface{})
	if !ok {
		return eventCounts, eventSubtypes
	}

	for _, opRaw := range opsRaw {
		op, ok := opRaw.(map[string]interface{})
		if !ok {
			continue
		}
		desc, _ := op["description"].(string)
		if desc == "" {
			continue
		}

		var eventType string
		if idx := strings.Index(desc, "("); idx > 0 {
			eventType = strings.TrimSpace(desc[:idx])
		} else {
			parts := strings.Fields(desc)
			if len(parts) > 0 {
				eventType = parts[0]
			} else {
				eventType = "Unknown"
			}
		}
		eventCounts[eventType]++

		if eventType == "osd_op" {
			bracketRe := regexp.MustCompile(`\[([^\]]+)\]`)
			if m := bracketRe.FindStringSubmatch(desc); m != nil {
				parts := strings.FieldsFunc(m[1], func(r rune) bool {
					return r == ',' || r == ' '
				})
				if len(parts) > 0 {
					subtype := strings.TrimRight(strings.TrimRight(strings.TrimRight(parts[0], ","), ":"), ".")
					eventSubtypes[subtype]++
				}
			} else {
				// Fallback: extract from :::pattern
				fallbackRe := regexp.MustCompile(`:::(\w+)[\.%]`)
				if m := fallbackRe.FindStringSubmatch(desc); m != nil {
					eventSubtypes[m[1]]++
				}
			}
		}
	}
	return eventCounts, eventSubtypes
}

func listClients(data map[string]interface{}) (map[string]int, map[string]int, map[int]int) {
	clients := make(map[string]int)
	clientIPs := make(map[string]int)
	clientTIDs := make(map[int]int)

	opsRaw, ok := data["ops"].([]interface{})
	if !ok {
		return clients, clientIPs, clientTIDs
	}

	for _, opRaw := range opsRaw {
		op, ok := opRaw.(map[string]interface{})
		if !ok {
			continue
		}

		typeData, ok := op["type_data"].(map[string]interface{})
		if !ok {
			continue
		}

		clientInfo, ok := typeData["client_info"].(map[string]interface{})
		if !ok {
			clientInfo = map[string]interface{}{"client": "internal"}
		}

		clientName, _ := clientInfo["client"].(string)
		if clientName == "" {
			clientName = "internal"
		}
		clients[clientName]++

		clientAddr, _ := clientInfo["client_addr"].(string)
		if clientAddr == "" || clientAddr == "unknown" {
			clientIPs["internal"]++
			continue
		}

		parts := strings.Split(clientAddr, "/")
		if len(parts) == 2 {
			ipPort := parts[0]
			connID, _ := strconv.Atoi(parts[1])
			ip := ipPort
			if colonIdx := strings.LastIndex(ipPort, ":"); colonIdx > 0 {
				ip = ipPort[:colonIdx]
			}
			clientIPs[ip]++
			clientTIDs[connID]++
		} else {
			clientIPs[clientAddr]++
		}
	}
	return clients, clientIPs, clientTIDs
}

func evaluateSimpleExpression(expr string) bool {
	expr = strings.TrimSpace(expr)

	if strings.Contains(expr, " AND ") {
		parts := strings.Split(expr, " AND ")
		for _, p := range parts {
			if !evaluateSimpleExpression(strings.TrimSpace(p)) {
				return false
			}
		}
		return true
	}
	if strings.Contains(expr, " OR ") {
		parts := strings.Split(expr, " OR ")
		for _, p := range parts {
			if evaluateSimpleExpression(strings.TrimSpace(p)) {
				return true
			}
		}
		return false
	}

	operators := []string{
		">=", "<=", "!=", "==", ">", "<",
	}

	var op string
	var parts []string
	for _, candidate := range operators {
		if strings.Contains(expr, candidate) {
			op = candidate
			parts = strings.SplitN(expr, candidate, 2)
			break
		}
	}

	if op == "" || len(parts) != 2 {
		return false
	}

	lhsStr := strings.TrimSpace(parts[0])
	rhsStr := strings.TrimSpace(parts[1])

	lhs, err := strconv.ParseFloat(lhsStr, 64)
	if err != nil {
		return false
	}
	rhs, err := strconv.ParseFloat(rhsStr, 64)
	if err != nil {
		return false
	}

	switch op {
	case ">":
		return lhs > rhs
	case ">=":
		return lhs >= rhs
	case "<":
		return lhs < rhs
	case "<=":
		return lhs <= rhs
	case "==":
		return lhs == rhs
	case "!=":
		return lhs != rhs
	}
	return false
}

func deepGet(data map[string]interface{}, path string) interface{} {
	parts := strings.Split(path, ".")
	current := data

	for i, part := range parts {
		// Handle array indexing like "events[0]"
		arrayRe := regexp.MustCompile(`^(\w+)\[(\d+)\]$`)
		if m := arrayRe.FindStringSubmatch(part); m != nil {
			key := m[1]
			idx, _ := strconv.Atoi(m[2])

			val, ok := current[key]
			if !ok {
				return nil
			}
			arr, ok := val.([]interface{})
			if !ok || idx >= len(arr) {
				return nil
			}

			if i == len(parts)-1 {
				return arr[idx]
			}

			next, ok := arr[idx].(map[string]interface{})
			if !ok {
				return nil
			}
			current = next
			continue
		}

		val, ok := current[part]
		if !ok {
			return nil
		}

		if i == len(parts)-1 {
			return val
		}

		next, ok := val.(map[string]interface{})
		if !ok {
			return nil
		}
		current = next
	}
	return nil
}

// FormatOperation formats a single operation for display.
func FormatOperation(op map[string]interface{}) string {
	duration, _ := toFloat64(op["duration"])

	var typeName string
	if td, ok := op["type_data"].(map[string]interface{}); ok {
		typeName, _ = td["type"].(string)
	}
	if typeName == "" {
		typeName, _ = op["type"].(string)
	}
	if typeName == "" {
		typeName = "Unknown"
	}

	description, _ := op["description"].(string)
	if len(description) > 80 {
		description = description[:77] + "..."
	}

	return fmt.Sprintf("%.2f ms - %s(%s)", duration, typeName, description)
}

func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// RunTestsFromDirectory runs all YAML patterns in a directory.
func (a *Analyzer) RunTestsFromDirectory(directory string, osdID int, dryRunData map[string]interface{}, overrideSource string, noFindings bool) {
	files, err := os.ReadDir(directory)
	if err != nil {
		fmt.Printf("🚫 Error reading directory: %v\n", err)
		return
	}

	var yamlFiles []string
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".yaml") || strings.HasSuffix(file.Name(), ".yml") {
			yamlFiles = append(yamlFiles, filepath.Join(directory, file.Name()))
		}
	}

	if len(yamlFiles) == 0 {
		fmt.Println("🚫 No test files found in the directory.")
		return
	}

	fmt.Printf("🔍 Found %d test(s) to run...\n\n", len(yamlFiles))

	for _, file := range yamlFiles {
		pattern, err := LoadPattern(file)
		if err != nil {
			fmt.Printf("Error loading %s: %v\n", file, err)
			continue
		}
		fmt.Println(colors.Wrap(fmt.Sprintf("🧩 Running spec: %s", filepath.Base(file)), colors.Purple))
		a.RunPattern(pattern, osdID, dryRunData, overrideSource, noFindings)
		fmt.Println(strings.Repeat("-", 60))
	}
}
