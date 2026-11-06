---
name: mongodb-logstat
description: Run and interpret mongodb-logstat, a Go CLI that reads MongoDB / Percona Server for MongoDB structured JSON logs (5.0 and later, mongod and mongos, plain, gzip, syslog/journald or Kubernetes pod logs, one file or a whole cluster dump) and reports process and replica-set events, grouped warnings and errors, slow-operation timelines, ranked query shapes, slow operations per client/user, and redacted AI-ready analysis JSON. Use when the user has mongod/mongos logs for a support case and wants to know what happened and when, which node restarted, stepped down or lost its sync source, which warnings or errors were logged, which queries were slow, which client sent them, or which mongodb-logstat report or flags to use; also when they paste mongodb-logstat output to explain, or when changing mongodb-logstat code or docs. Not for FTDC diagnostic.data (use mongodb-ftdcstat) or for sanitizing customer data (use mongodb-log-obfuscator-go).
---

# mongodb-logstat

Reads MongoDB structured JSON logs and prints terminal reports or JSON. It
takes files and directories (recursively): point it at a whole case folder
and it reads every MongoDB log in it, skips everything else (FTDC, YAML,
pt-stalk output, empty files), counts a log and its `.gz` copy once, and
attributes every file to the node (mongod/mongos) that wrote it.

## Privacy first

Customer logs contain hostnames, IPs, user names, namespaces, and query
values. For support cases, sanitize with `mongodb-log-obfuscator-go` first
unless the user says the data is not sensitive. `analysis` never prints
query values; `messages` samples replace query-like attributes with type
markers but keep error text and host names; `events` print host names,
replica-set names and paths as logged.

## Locating the binary

1. `command -v mongodb-logstat`
2. Otherwise the repo checkout, e.g. `~/bin/mongodb-logstat/mongodb-logstat`
3. Otherwise build it **into a scratch path**: `go build -o /tmp/mongodb-logstat .`
   from the repo root. Do not run `go build` or `go build ./...` in the repo
   to check compilation: the repo has a single main package, so that
   overwrites the user's binary. Use `go vet ./...` / `go test ./...`.

If you changed the source in this session, rebuild before running.

## Usage

```text
mongodb-logstat --report timeline|queries|clients|analysis|events|messages [--json] [--from TIME] [--to TIME] [--bucket 1h] [--group-by optype|namespace|query|category|node] [--top N] [--sort-by total-duration|count|p95|max|docs-examined] [--include-internal] [--fill-gaps] [--max-buckets N] [--verbose] <file-or-dir> [...]
```

### Input rules

- Supported: structured JSON logs, MongoDB 5.0 and later (tested on 6.0,
  7.0, 8.0, 8.2). A 4.2-or-older text log fails with that reason; if nothing
  can be read the tool exits 1 and lists why per file.
- Read as found: `.gz` (by content, not name), Kubernetes pod logs with a
  shell preamble, `kubectl logs --timestamps` / `docker logs -t` prefixes,
  syslog/journald lines (`Nov 02 10:13:11 host mongod[2619]: {...}`).
- Directories: non-log files are skipped (`Skipped files: N`, listed with
  `--verbose`). A file named on the command line is never skipped.
- Copies: a file that starts with the same lines as one already read (log
  next to its `.gz`, an earlier snapshot) only adds its new lines; the header
  shows `Duplicate lines: N skipped (copy repeats original)`.
- `--from` is inclusive, `--to` exclusive; RFC3339 or a time without zone,
  read as UTC (`2026-11-04T13:30`, `2026-11-04`). They apply to every report.

### Flag rules

- `--report` is required. `analysis` is always JSON.
- `--top`: default 20 for `queries`, query timelines and `analysis`; all for
  `clients` and `messages`; `0` means all. `events` ignores it.
- `--group-by` and `--bucket` are for `timeline` (`--bucket` also sets the
  `analysis` timeline). `--group-by node` gives one column per mongod/mongos.
- `--sort-by` ranks `queries`, `clients` (default `count`) and `analysis`.
- Exit 0 for a report (also when nothing matched), 1 when no input file could
  be read, 2 for usage errors.

## Picking a report

| Question | Command |
|---|---|
| What is in this case folder, which nodes, versions, time ranges | any report: read the header (`Nodes` table) |
| What happened and when: restarts, shutdowns, signals, unclean shutdown, fatal, state changes, elections, sync source, reconfig, initial sync, FCV | `--report events` |
| Which warnings / errors / fatals were logged, how often, where | `--report messages` |
| When were operations slow, on which node | `--report timeline --group-by node --bucket 15m` |
| Which kinds of operations were slow over time | `--report timeline --group-by optype` (or `namespace`, `query`, `category`) |
| Which query shapes cost the most | `--report queries --top 20` |
| Which user / client IP / application sent the slow operations | `--report clients` |
| Evidence for one window: shapes, plans, scan ratios, indicators, hot windows | `--report analysis --from ... --to ...` |
| Exact values, filtering, counting | `--json` piped through `jq` (see references) |

### Working order when you run it yourself

Point it at the whole case folder once you know it contains this case's logs.
Large logs take time (roughly 25 s per GB of plain log and 3-4 min per GB
of gzip, per report), so run reports in the background for multi-GB inputs, and narrow
with `--from/--to` once you know the window.

1. **`events`** (text). Small: one line per event. It gives the nodes, the
   versions, restarts and replica-set changes, and the times to look at.
2. **`messages`** (text). One line per distinct warning/error, with count,
   first/last, and nodes. Read the `E` and `F` rows first, then `W` rows whose
   first/last bracket the incident.
3. **`timeline --group-by node`** with a bucket that fits the range (`15m` for
   a day, `1m` for an hour) to see when and where slow operations happened.
4. **`queries`** or **`analysis`** for the window you found (`--from/--to`).
   `analysis` is JSON for reading with `jq`; do not read it whole.
5. **`clients`** when the question is who sent the load.

Measured sizes on a 7-node, 3-day sharded cluster (25 rotated files,
850 k slow operations): text `events` 5 KB, `messages` 5 KB, `queries` 7 KB,
`clients` 8 KB, `timeline` (1 h buckets) 11 KB; their JSON is 2-10x larger
(`clients` 85 KB); `analysis` 210 KB. So read 1-3 as text, and `analysis`
only through `jq`. Use `--json` with `jq` for questions
like "how many times", "which node first", or "between which events":

```bash
L="mongodb-logstat --json"
# events of one kind, per node, in order
$L --report events DIR | jq -r '.events[] | select(.kind=="rsState") | [.time,.node,.detail] | @tsv'
# warnings/errors that started inside a window
$L --report messages DIR | jq -r '.messages[] | select(.first >= "2026-11-04T13:00" and .first < "2026-11-04T14:00") | [.severity,.logId,.count,.msg] | @tsv'
# top shapes in the hottest analysis window, by name
$L --report analysis --from 2026-11-04T13:00 --to 2026-11-04T15:00 DIR | jq -r '.hotWindows[0].dominantShapes[] | [.label,.cumulativeDurationMs,.durationSharePct] | @tsv'
```

More recipes and every JSON field are in [references/json.md](references/json.md).

## Reading output

- **Header first.** `Input files`, `Skipped files`, `Duplicate lines`, the
  `Nodes` table (node `host:port`, process, replica set, role, version,
  files, log range, slow ops), the input log range, and the slow-op counters.
  `Input status: partial` means some files failed; `--verbose` lists why.
- **Nodes** come from startup and log-rotation lines (`MongoDB starting`,
  `Process Details`, options, Build Info). A mongos log that begins at
  process start has no host line and is named by its file.
- **Slow operations are only what was logged**: operations above
  `slowms` (and sampled by `slowOpSampleRate`). Counts are not total
  workload, and cumulative duration overlaps across concurrent operations: it
  is not server utilization.
- **Operations**: normalized to `find`, `aggregate`, `insert`, `update`,
  `delete`, `getMore`, `command` (admin/DDL/index/sharding/user commands),
  `internal` (replication, heartbeats, hello/isMaster), and `other`.
  Internal operations are excluded from `queries` ranking unless
  `--include-internal`.
- **Query labels** are `operation:namespace#short-shape-id`.
  `sample_line`/`sampleLine` (`file:line`) points to the worst sample, so you
  can read the raw line with `sed -n '<line>p' <file>` (unzip `.gz` first).
- **clients**: operations are tied to users by exact connection (`ctx`), never
  by IP. `uncorrelated_slow_ops` are operations whose connection was opened
  before the log starts; on shards behind mongos that is often most of them.
- **events** state facts only, no verdicts. `rsState` is this node's own
  transition; `memberState` is another member as seen by this node.
- **messages** group by severity, component, log id, and message text, so the
  same id with different text are separate rows.

## Diagnostic patterns

Facts first, then a cause, with evidence from at least two sources:

- **Restart / crash:** a clean stop is `signal` (15, SIGTERM: something
  asked it to stop) then `shutdown exitCode 0`. A `start` followed by
  `uncleanShutdown` with no `shutdown` before it means the process died
  without logging: SIGKILL (often the kernel OOM killer), a crash, or a host
  reset. mongod cannot log SIGKILL. Check `messages` `F` rows, the last lines
  before the gap, and the host's kernel log for OOM.
- **Primary change:** `electionStart` / `electionWon` / `rsState` on the new
  primary, `rsState PRIMARY -> SECONDARY` on the old one, `memberState ...
  DOWN` on others. Line the times up across nodes with `events`.
- **Replication trouble on a secondary:** many `syncSource` changes,
  `messages` W `REPL` 21122 "Oplog fetcher stopped querying remote oplog with
  error" (read its sample `error`), `memberState` flips. Then FTDC
  (`mongodb-ftdcstat --view repl --verbose`) for lag and fetch rates.
- **Network / TLS:** `messages` W `NETWORK` 23234/23235 (certificates),
  4615610 "Failed to check socket connectivity", 23019 slow DNS.
- **Query timeouts / errors:** `messages` W `QUERY` 23798 "Plan executor
  error during find" (sample shows `MaxTimeMSExpired` and the plan stage, for
  example `COLLSCAN` with docsExamined), 20478 getMore errors. Then `queries`
  for that namespace and window.
- **Write stalls:** `messages` W `STORAGE` 22225 "Flow control is engaged",
  W `COMMAND` 20471 "instance is locked" (fsync lock, often backups).
- **Expensive queries:** `queries` by `total-duration`, then `analysis` for
  the window: `planEvidence`, `ratios` (docs examined per returned),
  `indicators` (`collection_scan_observed`, `high_scan_amplification_observed`,
  `disk_spill_observed`, `replanned_observed`).

## Answer format

```text
input: <nodes, versions, files, log range; skipped/duplicates/partial if any>
observations: <events/messages/slow-op facts with times and nodes>
interpretation: <likely cause, with confidence>
next checks: <exact mongodb-logstat or mongodb-ftdcstat command(s), or data to ask for>
```

Say what is missing (FTDC, other nodes' logs, the time zone the customer
used, `slowms`) instead of guessing. Logs show what happened and which
operations were slow; FTDC shows resource pressure. Correlate the windows:
`analysis` `hotWindows` and `timeline` buckets line up with
`mongodb-ftdcstat --from/--to`.

## Changing the tool

Single Go package at the repo root: `cli.go` (flags), `logformat.go` (format
detection, gzip, prefixes), `input.go` (directory walk), `parser.go` (line
parsing, slow-op normalization, copy detection), `nodes.go` (node identity),
`events.go`, `messages.go`, `clients.go`, `report.go` (timeline, queries,
analysis, header). Log ids are matched before message text. If you change a
report, flag, or JSON field, update `README.md` (`readme_test.go` checks the
usage line, report sections, flags and event kinds), this skill and its
`references/json.md`, the Codex skill in `.codex/skills/mongodb-logstat-skill`,
and the tests. Validate with `go vet ./...` and `go test ./...`, then run the
changed report on real logs from more than one version and compare with the
previous build.
