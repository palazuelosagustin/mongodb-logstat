# mongodb-logstat

`mongodb-logstat` summarizes MongoDB structured JSON logs. It provides human terminal reports of slow operations for quick review, a client correlation report, a time-ordered list of process and replica-set events, a grouped list of warnings and errors, and a bounded, redacted JSON `analysis` report intended as input for AI-assisted investigation.

## Requirements

- Go `1.22+`

## Build

```bash
go build -o mongodb-logstat .
```

## Usage

```text
mongodb-logstat --report timeline|queries|clients|analysis [--json] [--from TIME] [--to TIME] [--bucket 1h] [--group-by optype|namespace|query|category] [--top N] [--sort-by total-duration|count|p95|max|docs-examined] [--include-internal] [--fill-gaps] [--max-buckets N] [--verbose] <file-or-dir> [...]
```

Flags may appear before or after input paths. Inputs can be files, directories, or a mix of both.

Input handling:

- Supported: structured JSON logs from MongoDB / Percona Server for MongoDB 5.0 and later, from mongod and mongos (tested with 7.0 and 8.0 logs). A legacy plain-text log (MongoDB 4.2 or older) is reported as a failed file with that reason; if no file can be read, the command exits 1 and lists why.
- Container logs work as collected: shell lines before mongod's first JSON line (Kubernetes operator pods) count as malformed lines, and a timestamp prefix added by `kubectl logs --timestamps` or `docker logs -t` is ignored.
- Gzip-compressed logs (for example rotated `mongod.log.1.gz`) are decompressed while reading.
- Directories are walked recursively. Files that are not MongoDB logs, such as FTDC files in `diagnostic.data/`, are skipped: the header shows `Skipped files: N`, JSON shows `filesSkipped` (`inputFilesSkipped` in `analysis`), and `--verbose` lists them. A file named directly on the command line is never skipped; it fails if it is not a log.
- Copies of one log are counted once. When a file starts with the same lines as one already read (for example `mongod.log` next to `mongod.log.gz`, or an earlier and a later snapshot of the same file), only the lines past what was already read are used. The header shows `Duplicate lines: N skipped (copy repeats original)` and JSON shows `duplicateLinesSkipped`.
- Empty files (common in collection-tool output) are skipped like other non-log files.
- Files with the same name from different nodes are told apart by their parent directories (`rs01-0/mongod.log`, `rs01-1/mongod.log`) in the header and in sample line references.
- Slow operations are the `Slow query` lines (log id `51803`). Single-statement writes logged with `"type":"update"`/`"remove"` (6.0) are counted as `update`/`delete`; admin, index and DDL commands such as `createIndexes`, `listDatabases` or `collMod` are counted as `command`. Other messages that mention "slow", such as `Slow connection establishment`, are not counted. Slow update and delete commands are logged with namespace `<db>.$cmd` on 5.0+; the collection is taken from the command instead.
- `--from` and `--to` accept RFC3339 (`2026-11-04T13:30:00Z`, `2026-11-04T10:30:00-03:00`) or a time without a zone, which is read as UTC (`2026-11-04T13:30`, `2026-11-04 13:30:00`, `2026-11-04`).

Important behavior and defaults:

- `--report` is required and must be one of `timeline`, `queries`, `clients`, `analysis`, `events`, or `messages`.
- `--bucket 1h`.
- `--group-by optype`.
- `--top` defaults to `20` for `queries`, query-grouped timelines, and `analysis`; `clients` defaults to all correlated user/client groups unless `--top` is set; use `--top 0` for all shapes/groups.
- `--sort-by total-duration` for query ranking and `count` for client ranking. `clients` also supports `total-duration`, `p95`, `max`, and `docs-examined`.
- `--fill-gaps=true` for `analysis` timeline buckets.
- `--max-buckets 10000`.

## Reports

### `timeline`

Bucketed terminal or JSON report for spotting slow-operation time windows.

```bash
./mongodb-logstat --report timeline --bucket 30m --group-by optype /path/to/mongod.log
./mongodb-logstat --report timeline --bucket 30m --group-by namespace --top 10 /path/to/logs
./mongodb-logstat --report timeline --json /path/to/mongod.log > timeline.json
```

`--group-by optype` uses normalized operations such as `find`, `aggregate`, `getMore`, `update`, `delete`, `internal`, and `other`; raw internal command names such as `configTerm` are not exposed as workload columns.

### `queries`

Two-pass top query-shape report with exact counts and latency stats for selected shapes.

```bash
./mongodb-logstat --report queries /path/to/mongod.log
./mongodb-logstat --report queries --top 20 --sort-by total-duration /path/to/mongod.log
./mongodb-logstat --report queries --json /path/to/mongod.log > queries.json
```

Rows include `label`, `count`, `avg_ms`, `p95_ms`, `max_ms`, dominant `plan`, and `sample_line`. `sample_line` identifies the worst-duration sample contributing to the row. Query labels are `operation:namespace#short-shape-id`; labels never render `#null`, and colliding prefixes are extended deterministically.


### `clients`

Terminal or JSON report that correlates accepted logged slow operations to MongoDB connection, client metadata, and authentication events (matched by log id, including the 5.0+ `Successfully authenticated` message) by exact log context such as `ctx:"conn188052"`. It never attributes by client IP alone. Slow operations without matching in-range connection metadata are counted as uncorrelated; the terminal header shows `uncorrelated_slow_ops` separately from `omitted_slow_ops` (groups cut by `--top`) and states their share. On shards behind mongos most slow operations often run on pooled connections opened before the log starts, so a large uncorrelated share is expected there.

```bash
./mongodb-logstat --report clients /path/to/mongod.log
./mongodb-logstat --report clients --top 20 --sort-by count /path/to/logs
./mongodb-logstat --report clients --json /path/to/mongod.log > clients.json
```

The primary ranked unit is `{database user, client IP}`. Terminal output groups globally ranked rows under `user@authenticationDatabase` headings and shows deterministic operation columns, hiding columns that are zero for all displayed rows. JSON output uses the schema-version-1 envelope and includes per-row applications, drivers, latency, operation counts, top namespaces, sample line references, omitted groups, and uncorrelated slow-operation counts.

### `analysis`

Schema-version-2 JSON report for AI analysis. It is always JSON; `--json` is accepted but not required.

```bash
./mongodb-logstat \
  --report analysis \
  --bucket 30m \
  --top 20 \
  --sort-by total-duration \
  --from 2026-06-30T00:00:00Z \
  --to 2026-06-30T08:00:00Z \
  /path/to/mongodb/logs > mongodb-analysis.json
```

The report includes:

- explicit semantics stating that the dataset is logged slow operations, not total workload;
- requested, input-observed, and accepted slow-operation ranges;
- diagnostics for partial input, malformed lines, filtering, canonicalization fallback, and unassociated `getMore`;
- filled timeline buckets for FTDC correlation;
- hot windows ranked by cumulative slow-operation duration;
- ranked query shapes with globally unique `shapeKey` values, redacted structure, observed operations, cursor counts, latency, cumulative impact, optional metric aggregates, `planEvidence`, indicators, and worst sample reference;
- top namespaces, compact internal-operation summaries, and omitted-data accounting.

### `events`

Time-ordered list of process, replica-set and configuration events, one row per event with time (millisecond precision, UTC), file, kind, and the facts the log states. It reports facts only: no thresholds, levels, or verdicts. Logs from several nodes are merged in time order, so a cluster dump reads as one timeline.

```bash
./mongodb-logstat --report events /path/to/cluster-dump
./mongodb-logstat --report events --json --from 2026-11-04T13:00 --to 2026-11-04T14:00 /path/to/logs
```

| kind | log id | detail |
|---|---|---|
| `start` | 4615611 | host, port, pid, dbPath (mongod) |
| `version` | 23403 | server version (mongod and mongos) |
| `signal` | 23377 | signal number and name |
| `shutdown` | 23138 | exit code |
| `uncleanShutdown` | 22271 | detected at startup |
| `fatal` | any `"s":"F"` line | message, error/code when present |
| `rsState` | 21358 | this node's old -> new replica-set state |
| `memberState` | 21215, 21216 | another member's new state as seen by this node (`DOWN` for 21216) |
| `electionStart` | 4615652 | no primary seen within the election timeout |
| `electionWon` / `electionLost` | 21450 / 21434 | term / dry run |
| `syncSource` | 21088 | old -> new sync source |
| `rsConfig` | 21392 | set name, config version, term, member count |
| `initialSyncStart` / `initialSyncDone` | 21164 / 21163 | attempt / duration |
| `fcv` | 20459 | featureCompatibilityVersion being set |

JSON (`schemaVersion` 1) has `kinds` (count per kind) and `events[]` with `time`, `file`, `line`, `kind`, `logId`, `detail`, and `facts` (the attribute values behind the detail). `--top` does not apply.

### `messages`

Warning, error and fatal lines (`"s"` of `W`, `E`, `F`) grouped by severity, component, log id and message, with count, first and last time, and the files that logged them. Sorted by severity (fatal first), then count. `--top N` limits the groups shown; the header and JSON account for the omitted groups and lines.

```bash
./mongodb-logstat --report messages /path/to/logs
./mongodb-logstat --report messages --json --top 50 /path/to/logs
```

JSON (`schemaVersion` 1) has `lines`, `bySeverity`, `groups`, `omittedGroups`, `omittedLines`, and `messages[]` with `severity`, `component`, `logId`, `msg`, `count`, `first`, `last`, `files` (count per file), and `sample` (file, line, and attributes of the first occurrence). In the sample, values under query-like keys (`cmd`, `command`, `filter`, `q`, `u`, `pipeline`, `keyValue`, `doc`, and similar) are replaced by type markers, and strings longer than 300 bytes are cut. Error codes, error messages and host names are kept.

## Query Identity

Shape identity precedence is:

1. `attr.queryShapeHash`
2. `attr.planCacheShapeHash`
3. `attr.queryHash`
4. a local deterministic privacy-safe canonical hash

`planCacheKey` is stored as plan-variant evidence, not used as shape identity. When canonicalization exceeds limits, the tool hashes a reduced structural fallback and records canonicalization diagnostics.

## Redaction And Limits

The `analysis` report never emits raw log lines or literal query/update values. It preserves field paths, operators, command names, stage names, sort direction, projection fields, hints/collation/read/write concerns, and structural evidence while replacing values with type markers such as `?string`, `?number`, and `?date`.

Limits include a `16 MiB` logical line limit, canonical nesting depth `32`, object key limit `1000`, inspected array element limit `1000`, and canonical representation limit `1 MiB`.

## Slow-Log Semantics

Counts and rates represent accepted logged slow operations only. They may be sampled depending on MongoDB slow-operation configuration. Cumulative duration can overlap across concurrent operations and is not server utilization.

Analysis rates are emitted under `global.rates.overAnalysisRange` with an explicit denominator. If both `--from` and `--to` are set, the denominator is that selected range; otherwise it is the observed input log range, falling back to the accepted slow-operation range when needed. `--from` is inclusive and `--to` is exclusive.

## Partial Input

If one or more files fail but at least one file is read, terminal reports print:

```text
Input status: partial (1/3 files read successfully)
```

JSON reports include diagnostics. If no input file can be read successfully, the tool exits with code `1` and emits no report body.

## Test

```bash
go test ./...
go vet ./...
go test -race ./...
```
