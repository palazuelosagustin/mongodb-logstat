---
name: mongodb-logstat-skill
description: Analyze MongoDB slow-query JSON logs with the local mongodb-logstat CLI. Use when Codex needs to summarize logged slow operations, build timelines, inspect or explain query-shape reports, generate AI-ready analysis JSON, choose flags, write reports to files, or answer mongodb-logstat CLI usage questions without consulting --help.
---

# MongoDB Logstat

Use the local `mongodb-logstat` project in this repo to inspect MongoDB structured JSON slow-operation logs.

## Quick Start

Prefer the built binary when it exists in the repo root:

```bash
./mongodb-logstat --report timeline /path/to/mongod.log
```

If the binary is missing or stale, build it first:

```bash
go build -o mongodb-logstat .
```

Flags may appear before or after input paths.

## CLI Reference

Usage syntax:

```text
mongodb-logstat --report timeline|queries|clients|analysis [--json] [--from RFC3339] [--to RFC3339] [--bucket 1h] [--group-by optype|namespace|query|category] [--top N] [--sort-by total-duration|count|p95|max|docs-examined] [--include-internal] [--fill-gaps] [--max-buckets N] [--verbose] <file-or-dir> [...]
```

Accepted inputs:
- one or more files
- one or more directories
- mixed files and directories

Defaults:
- `--report` is required; there is no default report
- `--bucket 1h`
- `--group-by optype`
- `--top` unset means default top `20` for `queries`, query-grouped timelines, and `analysis`; `clients` defaults to all correlated user/client groups; `--top 0` means all
- `--sort-by total-duration` for queries and analysis; `clients` defaults to `count`
- `--include-internal false`
- `--fill-gaps true`
- `--max-buckets 10000`
- `--json false`
- `--verbose false`
- `--from` unset
- `--to` unset

Flag details:
- `--report`: required; `timeline`, `queries`, `clients`, or `analysis`
- `--json`: emit JSON for v1 reports; `analysis` is always JSON
- `--from`: inclusive RFC3339 lower time bound
- `--to`: exclusive RFC3339 upper time bound
- `--bucket`: positive Go duration string, such as `30m`, `1h`, or `2h`
- `--group-by`: `optype`, `namespace`, `query`, or `category`; used by `timeline`
- `--top`: non-negative integer; `0` prints all selected rows or columns
- `--sort-by`: query/client ranking key: `total-duration`, `count`, `p95`, `max`, or `docs-examined`
- `--include-internal`: include internal commands in main query ranking; internal activity is summarized separately by `analysis`
- `--fill-gaps`: emit zero-slow-operation timeline buckets in `analysis`
- `--max-buckets`: reject `analysis` output that would exceed this bucket count
- `--verbose`: print per-file failures to stderr when input is partial or failed

Validation rules:
- at least one input path is required
- `--bucket` must be a positive duration
- `--top` must be `0` or greater when supplied
- `--max-buckets` must be positive
- `--from` and `--to` must be valid RFC3339 timestamps
- if both are provided, `--from` must be less than or equal to `--to`

Exit behavior:
- exit code `0` for successful reports, including valid no-match reports
- exit code `1` when no input file can be read successfully
- exit code `2` for CLI usage or validation errors

## Report Selection

Choose `timeline` when the user wants time-based spikes or changes.
Common flags:
- `--bucket 30m` for 30-minute grouping
- `--group-by optype` for normalized operation columns such as `find`, `update`, `delete`, `getMore`, `internal`, `other`
- `--group-by namespace` for namespace columns
- `--group-by query` for query-shape columns
- `--group-by category` for `read`, `write`, `cursor`, `transaction`, `command`, `internal`, `other`
- `--top N` only when the user wants to cap columns; omit it for all optype/namespace/category groups

Choose `queries` when the user wants ranked slow query shapes and per-shape latency.
Common flags:
- `--top N` to limit rows; default is `20`
- `--top 0` to print every shape
- `--sort-by total-duration` to rank by cumulative slow duration
- `--sort-by count`, `p95`, `max`, or `docs-examined` for alternate rankings

Choose `clients` when the user wants slow operations correlated to MongoDB connection, client metadata, and authenticated users.
Common flags:
- `--top N` to limit user/client groups; omit it for all correlated groups
- `--sort-by count` to rank by slow-operation count
- `--sort-by total-duration`, `p95`, `max`, or `docs-examined` for alternate rankings

Choose `analysis` when the user wants compact AI-ready JSON without raw logs.
Common flags:
- `--bucket 30m` for FTDC correlation windows
- `--top 20` for bounded query shapes and namespaces
- `--sort-by total-duration` for impact-first ranking
- `--include-internal` only if internal commands should be in main query ranking
- `--max-buckets N` if the requested range is very large

## Default Commands

Use these commands when the user asks how to run each report type to an output file with default parameters:

```bash
./mongodb-logstat --report timeline /path/to/mongod.log > timeline_report.txt
./mongodb-logstat --report queries /path/to/mongod.log > queries_report.txt
./mongodb-logstat --report clients /path/to/mongod.log > clients_report.txt
./mongodb-logstat --report analysis /path/to/mongod.log > analysis_report.json
```

JSON v1 reports:

```bash
./mongodb-logstat --report timeline --json /path/to/mongod.log > timeline_report.json
./mongodb-logstat --report queries --json /path/to/mongod.log > queries_report.json
./mongodb-logstat --report clients --json /path/to/mongod.log > clients_report.json
```

Capture stderr separately when needed because partial-input details go to stderr:

```bash
./mongodb-logstat --report timeline --verbose /path/to/mongod.log > timeline_report.txt 2> timeline_errors.txt
```

## Common Command Patterns

```bash
./mongodb-logstat --report timeline --bucket 30m --group-by optype /path/to/mongod.log
./mongodb-logstat --report timeline --bucket 30m --group-by query --top 10 /path/to/mongod.log
./mongodb-logstat --report queries --top 10 --sort-by total-duration /path/to/mongod.log
./mongodb-logstat --report queries --top 0 --sort-by count /path/to/mongod.log
./mongodb-logstat --report clients --top 20 --sort-by count /path/to/mongod.log
./mongodb-logstat --report analysis --bucket 30m --top 20 /path/to/mongod.log > mongodb-analysis.json
./mongodb-logstat --report analysis --from 2026-07-14T21:00:00Z --to 2026-07-14T23:00:00Z /path/to/mongod.log > mongodb-analysis.json
```

## Output Semantics

Keep these behaviors in mind when explaining results:
- `timeline`, `queries`, `clients`, and `analysis` use normalized operations; raw internal command names such as `configTerm` do not appear as normal workload operations.
- `timeline` and `queries` are two-pass; `analysis` emits schema-version-2 JSON.
- Query identity precedence is `queryShapeHash`, `planCacheShapeHash`, `queryHash`, then local deterministic privacy-safe canonical hash.
- `planCacheKey` is plan-variant evidence, not query identity.
- Query labels are `operation:namespace#short-shape-id`; they never render `#null`.
- Associated `getMore` latency contributes to the originating query shape when `originatingCommand` is available, but operation timelines still count it as `getMore`.
- `queries.sampleLine` or terminal `sample_line` points to the worst-duration sample for that row.
- Oversized logical lines above `16 MiB` are skipped and counted as malformed.
- If some inputs fail but at least one succeeds, the tool reports partial input.
- Counts and rates represent logged slow operations only; they may be sampled and do not prove total workload. Analysis rates include the explicit denominator under `global.rates.overAnalysisRange`.
- Cumulative duration can overlap across concurrent operations and is not server utilization.

## Analysis JSON Contents

The `analysis` report includes:
- `schemaVersion: 2` and `report: analysis`
- `semantics` with slow-log limitations and privacy guarantees
- `ranges` for requested, input-observed, and slow-operation-observed time spans
- `diagnostics` for input status, malformed lines, filtered records, canonicalization fallback, and unassociated `getMore`
- `global` latency, bounded counts, cumulative duration, structured indicators, and denominator-qualified rates
- `timeline` filled buckets by default
- `hotWindows` for FTDC correlation
- `queryShapes` with globally unique `shapeKey` values, redacted shape, observed operations, cursor counts, impact, optional metrics, `planEvidence`, ratios, indicators, and worst sample reference
- `namespaces`, `internalOperations`, `omitted`, and `indicatorThresholds`

Redaction preserves field paths/operators/stages/sort structure and replaces values with type markers such as `?string`, `?number`, and `?date`. Do not claim values are available from `analysis`.

## Interpretation Hints

Use these shortcuts when helping a user read reports:
- In `timeline`, columns after `start` and `total` are selected group values for `--group-by`.
- In `queries`, `plan` is the dominant rendered plan summary for that shape.
- In `queries`, `avg_ms`, `p95_ms`, and `max_ms` are milliseconds.
- In `analysis.queryShapes[].impact`, `totalDurationMs` ranks cumulative logged slow-operation impact.
- In `analysis.queryShapes[].execution`, metric averages only include records where the metric was present; missing metrics are not coerced to zero.
- For suspected resource pressure, use `analysis.hotWindows` as time windows to correlate with FTDC; do not infer CPU, disk, memory, or replica health from slow logs alone.

## Working Pattern

When asked to analyze a log or explain how to run the tool:
1. Pick `timeline` for time windows, `queries` for top shapes, or `analysis` for AI-ready evidence.
2. Prefer default commands unless the user asked for filters, JSON, or specific bucket/top values.
3. Use shell redirection when the user wants an output file.
4. Quote the exact command used when asked how a report was produced.
5. Use `sample_line` to point back to a raw log sample when the user asks about a specific query row.
6. Answer CLI usage questions from this skill content rather than invoking `--help` unless validating a changed binary.
