# mongodb-logstat

`mongodb-logstat` summarizes MongoDB structured JSON slow-operation logs. It provides human terminal reports for quick review, a client correlation report, and a bounded, redacted JSON `analysis` report intended as input for AI-assisted investigation.

## Requirements

- Go `1.22+`

## Build

```bash
go build -o mongodb-logstat .
```

## Usage

```text
mongodb-logstat [--report timeline|queries|clients|analysis] [--json] [--from RFC3339] [--to RFC3339] [--bucket 1h] [--group-by optype|namespace|query|category] [--top N] [--sort-by total-duration|count|p95|max|docs-examined] [--include-internal] [--fill-gaps] [--max-buckets N] [--verbose] <file-or-dir> [...]
```

Flags may appear before or after input paths. Inputs can be files, directories, or a mix of both.

Important defaults:

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

Terminal or JSON report that correlates accepted logged slow operations to MongoDB connection, client metadata, and authentication events by exact log context such as `ctx:"conn188052"`. It never attributes by client IP alone. Slow operations without matching in-range connection metadata are counted as uncorrelated.

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
