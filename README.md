# Ceph Companion (Go)

A unified Go implementation of Infomaniak's Ceph infrastructure automation
tools, combining `ceph_companion` (Prometheus metrics + automated
remediation) and `ceph_analyzer` (YAML pattern-based OSD analysis) into a
single binary.

## Contents

- [Overview](#overview)
- [Installation](#installation)
- [Configuration](#configuration)
- [`get` — Prometheus metrics](#get--prometheus-metrics)
- [`analyze` — OSD pattern analysis](#analyze--osd-pattern-analysis)
- [`operator` — rule engine](#operator--rule-engine)
- [Project structure](#project-structure)
- [Development](#development)
- [Safety notes](#safety-notes)

## Overview

Three subcommands, one binary:

| Subcommand | Purpose | Backed by |
|---|---|---|
| `get` | Ad-hoc Prometheus queries for cluster health metrics | [internal/prometheus](internal/prometheus) |
| `analyze` | Deep, per-OSD analysis via a small YAML pattern DSL (`ceph tell osd.N ...`) | [internal/osdanalyzer](internal/osdanalyzer) |
| `operator` | Automated remediation: YAML rules chain conditions, then run a shell action if all pass | [internal/operator](internal/operator) |
| `doctor` | Sanity checks: exec commands in PATH, Prometheus endpoint and exporter scrape for this host, notification webhook (opt-in) | [internal/cli/doctor.go](internal/cli/doctor.go) |

All three read the same [config.yaml](#configuration) for cluster endpoints,
default thresholds, and notification settings.

## Installation

```bash
make build          # -> bin/ceph-companion
make install         # go install ./cmd/ceph-companion
# or:
go install ./cmd/ceph-companion

# from the module path (any directory):
go install github.com/infomaniak/ceph-companion/cmd/ceph-companion@latest
```

## Configuration

The configuration file is a single explicit path — `--config` (global flag,
default `/etc/ceph-companion/config.yaml`, shown in `--help`), overridable
via `CEPH_COMPANION_CONFIG`. There is no `./config.yaml` auto-detection.

`rules/` and `patterns/` directories are searched in this order, so a
system-wide install (binary in `/usr/local/bin`, run from any directory)
doesn't need everything colocated:

1. Explicit override — `operator --rules-dir` / `analyze --patterns-dir`
   (used strictly: a missing directory is an error).
2. `./rules` / `./patterns` (current working directory) — only when the
   flag is left at its default.
3. `rules` only: next to the binary itself.
4. `/etc/ceph-companion/rules` / `/etc/ceph-companion/patterns` (the flag
   defaults, shown in `--help`).

`rules/` additionally auto-creates itself at `/etc/ceph-companion/rules`
with a placeholder sample rule if nothing is found anywhere (requires
write access there — typically running as root). `patterns/` has no
auto-create; if nothing is found, you'll see "no patterns found"-style
messages same as always.

See [etc/ceph-companion/configs/config.yaml](etc/ceph-companion/configs/config.yaml) for a fully-commented example.

```yaml
prometheus:
  url: "http://prometheus.example.com:9090"
                                      # any subpath in the URL is preserved
                                      # (e.g. VictoriaMetrics: "https://host/prometheus")
  cluster_id: "your-cluster-id"       # mandatory: value of the cluster label on ceph
                                      # metrics (e.g. the fsid); every query is scoped
                                      # to it via {cluster='<id>'}
  # cluster_label: "cluster"          # Prometheus label carrying cluster_id
                                       # (default: "cluster"; set this if an exporter
                                       # uses e.g. "ceph_cluster" or "job")
  # username: "optional"                # basic auth credentials, for endpoints
  # password: "optional"                # secured behind a reverse proxy

notifications:
  webhook: "https://kchat.infomaniak.com/hooks/..."   # kChat-compatible webhook
  channel: "#ceph-alerts"
  username: "ceph-companion"    # optional, defaults shown
  emoji: ":ceph:"              # optional
  # error_channel: "...-debug"          # optional: execution errors go to a
  # error_webhook: "https://.../hooks/2" # dedicated spam-tolerant channel;
                                        # both fall back to channel/webhook
```

Notes:
- `url` and `cluster_id` are mandatory. The endpoint may host metrics for
  several Ceph clusters; every query is scoped to the configured cluster
  via `cluster_id` (the `{cluster_selector}`/`{cluster_id}` placeholders in
  rule queries expand from it too). There is no auto-detection — the ID
  must be pinned in config.
- **Execution errors are never silent**: a condition that cannot produce a
  verdict (Prometheus query failure, missing metric data, missing
  `cephadm`/`journalctl`, failed ok-to-stop check), a failed live action,
  or a missing rules directory each produce one batched kChat message per
  run, sent in dry-run and live mode alike. Regular action notifications
  stay on `channel`; route error notifications elsewhere via
  `error_channel`/`error_webhook`. The only blind spot left is a broken
  `config.yaml` (which hides its own webhook) or a dead notification
  service.
- `notifications` is optional — omit it (or leave `webhook` empty) to
  disable notifications entirely; the operator just skips sending them.

## `get` — Prometheus metrics

```bash
ceph-companion get osd-latency
ceph-companion get slow-ops
ceph-companion get osd-performance-drop
ceph-companion get pool-performance-drop
ceph-companion get rgw-performance-drop
ceph-companion get bluestore-slow-committed-kv-count
```

Flags: `--debug` (print queries/errors to stderr). `osd-latency` also accepts
`--alarm` (exit code 3 if `--threshold`, default 1000ms, is exceeded — handy
for monitoring systems like Zabbix that key off exit codes) and
`--threshold <ms>`. `bluestore-slow-committed-kv-count` accepts `--window
<dur>` (default `1h`) for the PromQL lookback. `osd-performance-drop` /
`pool-performance-drop` / `rgw-performance-drop` accept `--threshold <pct>`
(default 30) and reuse the same drop-detection logic as the operator's
`osd_performance_drop`/`pool_performance_drop`/`rgw_performance_drop`
conditions ([internal/operator/cond_performance_drop.go](internal/operator/cond_performance_drop.go)).

## `analyze` — OSD pattern analysis

Runs YAML-defined "patterns" against live (or mocked) per-OSD Ceph data:

```bash
ceph-companion analyze --osd-id 963 --file patterns/slow_commit_check.yaml
ceph-companion analyze --osd-id all --patterns-dir patterns/
ceph-companion analyze --test slow_HDD,osd_scrubs           # by name, from ./patterns
ceph-companion analyze --mock-data --file patterns/slow_commit_check.yaml   # no cluster needed
ceph-companion analyze --no-findings --patterns-dir patterns/         # suppress the findings section
```

Flags: `--osd-id <n>|all`, `--file <path>`, `--patterns-dir <path>`, `--test
<name>[,<name>...]`, `--source <osd_historic_ops|osd_perf_dump|osd_ops_in_flight|osd_counter_dump|prometheus>`
(override the pattern's own `source:`), `--mock-data`, `--no-findings`.

## `doctor` — environment sanity checks

```bash
ceph-companion doctor                 # paths + prometheus checks
ceph-companion doctor --webhook       # additionally send a test message to the notification webhook(s)
```

`doctor` starts with a header (`version`, `config` path, `hostname`,
`cluster_id`), then runs:

- **paths** — every external command used in exec contexts (`cephadm`,
  `journalctl`, `lsblk`, `smartctl`, `sh`) is available in PATH
- **prometheus** — the configured endpoint answers, the ceph exporter
  scrapes this host (`up` per job for `instance == hostname`), and
  `ceph_bluestore_slow_committed_kv_count` has series for this host; a
  zero here is diagnosed against the cluster-wide count to distinguish
  "this host isn't scraped" from "metric missing entirely"
- **webhook** (with `--webhook`) — sends a labeled action test message to
  the regular target and a labeled error test message via the error path
  (the dedicated `error_webhook` when configured, otherwise the regular
  webhook with `error_channel` routing)

Exit code 0 when all selected checks pass, 1 otherwise.
Patterns with `source: prometheus` use the `prometheus` section of
config.yaml.

### Data sources

`ceph`-based sources (`osd_historic_ops`, `osd_perf_dump`, `osd_ops_in_flight`,
`osd_counter_dump`) run through `cephadm shell ceph tell osd.N ...` — OSD nodes
have no `client.admin` keyring in `/etc/ceph`, so a bare `ceph tell` fails to
authenticate. Fetches are cached per (source, OSD): a directory of 19 pattern
files costs one `cephadm shell` invocation per source, and fetch failures print
the actual command stderr instead of a bare "no data".

In `osd_counter_dump`/`osd_perf_dump` JSON, a counter is either a plain
number (e.g. `num_scrubs_started_ec: 170`) or a
`{"avgcount": N, "sum": S, "avgtime": T}` map (latency counters). Token
semantics for dotted paths into that JSON:

- `sub.counter.avgtime` → the daemon-reported lifetime average (seconds);
  unresolvable while `avgcount == 0` (nothing averaged yet). Older daemons
  that only emit `{avgcount, sum}` get the average computed as `sum/avgcount`.
- bare `sub.counter` → the number as-is; a counter map resolves to its
  cumulative `sum`.
- `sub.counter.avgcount` / `sub.counter.sum` → the literal map keys.

Note that scrub bookkeeping counters (`num_scrubs_started_ec`,
`successful_scrubs_ec_elapsed`, ...) live in the `osd` subsystem
(`osd[0].counters.*`), split by pool type only (ec/replicated, no
deep/shallow split); the `osd_scrub_{dp,sh}_{ec,repl}` subsystems hold the
chunk-level counters (`preemptions`, `chunk_busy`, `write_blocked_by_scrub`,
...) — see [etc/ceph-companion/patterns/osd_scrubs.yaml](etc/ceph-companion/patterns/osd_scrubs.yaml).

`source: prometheus` queries the configured Prometheus endpoint (from
config.yaml) instead of shelling out, scoped to
the target OSD via `ceph_daemon='osd.N'` (falling back to `osd='N'`) plus the
cluster label. Two token forms are supported in patterns:

- raw exporter metric names: `ceph_bluestore_slow_committed_kv_count > 0`
  (instant value),
- perf-dump-style dotted paths, auto-mapped to the exporter's `_sum`/`_count`
  gauge pairs so existing patterns work unchanged:
  `bluestore.state_done_lat.avgtime` →
  `ceph_bluestore_state_done_lat_sum / ceph_bluestore_state_done_lat_count`
  (same lifetime-average semantics as perf dump), `sub.counter.avgcount` →
  `..._count`, `sub.counter.sum` → `..._sum`, bare `sub.counter` → the gauge.

The ceph-JSON-only functions (`TOP`, `event_duration`, `count`, `list_*`,
`scrub_blocked_by`) are rejected with a clear message under
`source: prometheus` — use a ceph source, or an operator `prometheus_query`
rule for rate/increase-based checks.

### Pattern file format

```yaml
name: my_pattern            # optional, defaults to the filename
source: osd_historic_ops    # osd_historic_ops | osd_perf_dump | osd_ops_in_flight | osd_counter_dump | prometheus
patterns:
  - "event_duration('commit') > 100"
findings:
  - "Detected commits taking longer than 100ms: {event_duration('commit')}ms"
```

`source` selects which `cephadm shell ceph tell osd.N ...` command supplies
the data (`dump_historic_ops`, `perf dump`, `dump_ops_in_flight`,
`counter dump` respectively), or `prometheus` to query the cluster's
Prometheus endpoint instead. Each string in `patterns` is evaluated by
[`EvaluatePattern`](internal/osdanalyzer/analyzer.go); if any pattern succeeds,
every string in `findings` is printed with `{...}` placeholders substituted
from every pattern's found values in that file.

### Pattern DSL reference

| Expression | Meaning |
|---|---|
| `TOP(n, ops)` / `TOP(n, ops, 'filter')` | Top-`n` slowest ops (by duration), optionally filtered by substring match on the op description |
| `event_duration('event')` / `event_duration('event', 'filter')` | Max duration of a named event across all ops (optionally filtered) — combine with a comparison, e.g. `> 100` |
| `count(events)` / `count(events, 'filter')` | Count of ops/in-flight entries, optionally filtered — combine with a comparison |
| `list_events()` | Tallies of top-level event types and `osd_op` subtypes |
| `list_clients()` | Tallies of client names and source IPs seen in the ops |
| `scrub_blocked_by(n)` | Ops blocked waiting on scrub; the value is the count waiting on the single most-contended object, gated to 0 unless at least `n` ops wait on that same object — e.g. `scrub_blocked_by(3) > 0` fires only when 3+ ops pile up on one object. Findings identify that object and classify it as an RGW bucket-index or user-bucket object. The numeric argument is required |
| `path.to.metric > 0.001` | Any dotted path into the source JSON (supports `key[n]` array indexing), compared against a literal **or another metric path** — e.g. `osd[0].counters.num_scrubs_started_ec > osd[0].counters.successful_scrubs_ec`. Counter maps (`{avgcount, sum, avgtime}`) resolve via the rules above: `.avgtime` = lifetime average, bare name = `sum` |
| `ceph_some_metric > 0` | With `source: prometheus`: the instant value of an exporter metric for the target OSD, compared against a literal or another metric |

Comparisons support `>`, `>=`, `<`, `<=`, `==`/`=`, `!=`, and can be combined
with ` AND ` / ` OR `. If any metric path referenced can't be resolved,
evaluation is skipped (not silently compared against 0) and the missing
path is reported.

Nineteen ready-made patterns ship in
[etc/ceph-companion/patterns/](etc/ceph-companion/patterns/), covering
BlueStore slow ops, RocksDB/read/write latency, scrub contention, EC
sub-op latency, and client/event introspection.

## `operator` — rule engine

DRY-RUN by default; nothing executes until you pass
`--yes-i-really-mean-it`:

```bash
ceph-companion operator --list-rules
ceph-companion operator --debug                    # dry-run, all rules
ceph-companion operator --rule stop_sluggish_osd_service --debug # dry-run, one rule
ceph-companion operator --yes-i-really-mean-it     # live execution all rules
ceph-companion operator --rule stop_osd_with_hw_error --yes-i-really-mean-it  # live execution, one rule
```

Flags: `--rules-dir <path>` (default: `/etc/ceph-companion/rules`,
auto-created with a sample if missing when left at the default),
`--rule <name>`,
`--list-rules`, `--debug`, `--yes-i-really-mean-it`.

### Rule format

```yaml
name: my_rule
description: What this does and why
enabled: true

conditions:                 # evaluated in order, AND semantics - all must trigger
  - command: prometheus_query
    query: "topk(5, sort_desc(ceph_osd_apply_latency_ms{cluster_selector}))"
    operator: ">"            # >, >=, <, <=, ==, != (default: >)
    threshold: 1000
    entity_type: osd

action:
  type: ceph_cli
  command: "cephadm shell ceph osd ok-to-stop osd.{id} && cephadm unit stop --name osd.{id}"
  dry_run_text: "WOULD STOP: {full_name}"
  max_items_per_run: 1
  send-notif: true
  notif-message: "OSD {items} stopped ({count} total) on {host}/{cluster}"
```

`action.command`/`dry_run_text`/`notif-message` support `{id}`, `{full_name}`
/ `{name}` (`osd.N`), `{entity_type}`, `{osd_id}`, `{value}` (from the
matched item), and in `notif-message` also `{items}`, `{count}`, `{host}`,
`{cluster}`.

### Condition chaining

Each condition's item list **replaces** the previous one — except a
condition that legitimately has no items of its own (a cluster-wide gate
like a performance-drop check) leaves the prior items untouched rather than
wiping them out, so a later `osd_ok_to_stop` still has a target. A rule only
proceeds past a condition if it's `Triggered`; the final action runs against
whatever items are left once every condition has passed.

### Condition types

| `command` | What it does |
|---|---|
| `prometheus_query` | Generic: runs the PromQL in `query:` and compares each result to `threshold`/`operator`. **No Go code needed** for a new metric-based check. `{cluster_selector}` expands to a label matcher for the configured cluster (and is automatically narrowed to the OSD IDs an earlier condition already found, if any, via `input_items`); `{cluster_id}` expands to the bare cluster ID. See [etc/ceph-companion/rules/example_generic_query.yaml](etc/ceph-companion/rules/example_generic_query.yaml). |
| `osd_performance_drop` | Cluster-wide IOPS/bandwidth drop (5m vs 1h), OR across two metric ratios — not expressible as one PromQL comparison, so it stays a dedicated function. Produces no items (pure gate). **`threshold:` is required** — there's no safe built-in default for a percentage-drop gate (0% would trigger on any noise), so a rule that omits it fails validation before anything runs, with a clear error naming the rule and condition. |
| `pool_performance_drop` / `rgw_performance_drop` | Same shape and same required-`threshold:` rule, scoped to per-pool or RGW put/get metrics. |
| `kernel_disk_errors` | Scans kernel logs (`journalctl -k`) for I/O error keywords, then resolves the affected kernel device (e.g. `sda`, `nvme0n1`) to real Ceph OSD ID(s) via `lsblk`/WWN/serial correlation against `ceph device ls-by-host`, with a persistent cache for devices that have since been renamed/removed (see [internal/ceph/device.go](internal/ceph/device.go), [internal/operator/device_cache.go](internal/operator/device_cache.go)). Fields: `minutes` (default 60), `error_limit` (default 1), `rotational_only` (skip SSD/NVMe). Zero-capacity hits — "attempt to access beyond end of device" with `limit=0` — are ambiguous and gated on **cache corroboration**: a crashed disk drops to zero capacity (and often vanishes from `lsblk`, which is what the cache exists for), so the probes count when the device→OSD ownership survives scrutiny (live lsblk identity, or a cache record whose `osd_ids` reconciliation didn't contradict, or a ceph-table claim the cache corroborates); they are ignored — and the refusal journalled — when nothing corroborates it, which is the ghost gendisk of a replaced disk probed by `pvs`/`ceph-volume` under a name the rebuilt OSD reuses. Before attribution the cache is reconciled against live data: entries contradicted by the current `ceph device ls-by-host`/`lsblk` mappings are dropped, and every invalidation is logged on every run (`AlwaysLog`) — only positive conflicts invalidate, missing live data never does. Every trigger also journals its evidence: per-device hit counts, raw sample kernel lines, and how the OSD was attributed (`lsblk-wwn`, `ceph-device-map`, `cache-name-only`, …) — so a live-mode run is self-documenting without `--debug`. |
| `smart_media_errors` | Given OSDs from a prior condition (`input_items`), resolves each OSD's devices via `ceph osd metadata`, keeps only the **rotational** ones (the NVMe/SSD accelerator is skipped), runs `smartctl -x` on each and counts media-error indicators: SCSI/SAS error-log entries with a medium/hardware sense key (e.g. `[4,9,0] Successfully reassigned`, `[3,11,0] Require Write or Reassign Blocks command`), grown defect lists, **pending defect counts** (the SAS equivalent of ATA pending sectors — only printed by `-x`, which is why `-x` and not `-a`), and ATA reallocated/pending/uncorrectable sectors. Recovered background-scan rewrites ("Recovered via rewrite in-place") are shown in the summary but deliberately never counted — the data was corrected and healthy drives log them too. Triggers when an OSD's worst device reaches `error_limit` (default 1). Requires smartmontools on the OSD host; smartctl's non-zero exit (e.g. health FAILED) is still parsed. See [internal/ceph/smart.go](internal/ceph/smart.go). |
| `osd_ok_to_stop` | Given OSDs from a prior condition (`input_items`), checks each is safe to stop (`cephadm shell ceph osd ok-to-stop`) and not already stopped. Triggers only if **all** are safe. |

Every threshold lives where it's used — a rule condition's own `threshold:`,
or a `get` flag like `--threshold`/`--window` — rather than in a shared
config-level defaults block. There's no implicit fallback to reason about:
what's in the YAML (or the flag) is what runs.

Four rules ship in [etc/ceph-companion/rules/](etc/ceph-companion/rules/):
`stop_sluggish_osd_service` (BlueStore slow-KV + high latency + cluster
performance drop), `stop_osd_bluestore_slow_committed_kv_count` (same idea,
different condition ordering), `stop_osd_with_hw_error` (kernel disk errors
→ OSD resolution → ok-to-stop), and `example_generic_query` (disabled,
documentation only).

## Project structure

```
ceph-companion/
├── cmd/ceph-companion/          # main()
├── internal/
│   ├── cli/                     # Cobra commands: get, analyze, operator, root
│   ├── colors/                  # shared ANSI color codes for terminal output
│   ├── config/                  # config.yaml structs + loading, prometheus section validation
│   ├── prometheus/              # HTTP client, response types, label-selector building, result extraction
│   ├── ceph/                    # `ceph`/`cephadm`/`lsblk`/`journalctl` CLI wrappers
│   ├── osdanalyzer/             # pattern DSL evaluator (+ its pattern types)
│   └── operator/                # rule engine, rule/item types, condition evaluators, device-identity cache, kChat client
├── etc/ceph-companion/
│   ├── configs/config.yaml      # commented sample config
│   ├── patterns/                # analyze pattern library (19 files)
│   └── rules/                   # operator rule library (4 files)
```

## Development

```bash
make test    # go test -v ./...
make fmt     # go fmt ./...
make build   # -> bin/ceph-companion (static: CGO_ENABLED=0, version-stamped)
make deps    # go mod tidy && go mod download
```

Local builds are statically linked (`CGO_ENABLED=0 -trimpath`) and stamped
with the `git describe` version, commit hash, and build date — same recipe
as CI. `ceph-companion --version` reports them.

Testing philosophy: exec-wrapped functions (querying Prometheus, shelling
out to `ceph`/`lsblk`/`journalctl`) are kept thin, delegating to pure
parsing/logic functions that take raw input and are unit-tested directly —
see `internal/ceph/device_test.go` and `internal/operator/device_cache_test.go`
for the pattern (fixture strings in, parsed structs out, no subprocess
needed to test the logic).

## Safety notes

- The operator is **dry-run by default** everywhere; live execution requires
  the explicit `--yes-i-really-mean-it` flag.
- `osd_ok_to_stop` and the kernel-log-scanning device resolution exist
  specifically so an automated rule doesn't stop the wrong OSD — if you add
  a new rule that ends in a `ceph orch`/`cephadm` stop command, chain it
  through `osd_ok_to_stop` (or your own equivalent check) rather than
  acting directly on condition output.
- `kernel_disk_errors`' device→OSD resolution intentionally does **not**
  treat two devices with an unknown ("") serial number as a match — that
  would misattribute a disk error to an arbitrary cached OSD.
- In live mode the operator journals its own decision trail (triggered
  condition evidence, cache invalidations, each executed command and its
  outcome) so an automated stop can be explained from the systemd journal
  alone. Cache invalidations are logged on every run even when they prevent
  a trigger, since that's exactly when nothing else explains why.
