---
name: mongodb-logstat-skill
description: Analyze MongoDB slow-query JSON logs with the local mongodb-logstat CLI. Use when Codex needs to summarize slow operations, build time-bucketed timelines, inspect top query shapes, choose the right report and flags, or explain mongodb-logstat output for a mongod.log or directory of logs.
---

# MongoDB Logstat

Use the local `mongodb-logstat` project in this repo to inspect MongoDB slow-query logs.

## Quick Start

Prefer the built binary when it exists in the repo root:

```bash
./mongodb-logstat --report summary /path/to/mongod.log
```

If the binary is missing or stale, build it first:

```bash
go build -o mongodb-logstat .
```

Flags may appear before or after input paths.

## Report Selection

Choose `summary` when the user wants a fast high-level view:
- accepted operation count
- ops/min
- latency percentiles
- top operations and namespaces
- plan summary counts

Choose `timeline` when the user wants time-based spikes or changes.
Common flags:
- `--bucket 30m` for 30-minute grouping
- `--group-by optype` for operation columns such as `find`, `update`, `getMore`
- `--group-by namespace` for namespace columns
- `--group-by query` for query-shape columns
- `--top N` only when the user wants to cap columns; omit it to print every group

Choose `queries` when the user wants the most common slow query shapes and their latency.
Common flags:
- `--top N` to limit rows
- omit `--top` to print every shape

## Commands

Use these command patterns:

```bash
./mongodb-logstat --report summary /path/to/mongod.log
./mongodb-logstat --report timeline --bucket 30m --group-by optype /path/to/mongod.log
./mongodb-logstat --report queries --top 10 /path/to/mongod.log
```

For JSON output:

```bash
./mongodb-logstat --report summary --json /path/to/mongod.log
```

For time filtering:

```bash
./mongodb-logstat --report timeline --from 2026-07-14T21:00:00Z --to 2026-07-14T23:00:00Z /path/to/mongod.log
```

## Output Semantics

Keep these behaviors in mind when explaining results:
- `summary` is one-pass; `timeline` and `queries` are two-pass.
- `timeline` prints every group unless `--top` is set to a positive value.
- `queries` groups by an internal shape key, but visible `shapeId` is `null` when the server did not log `queryHash` or `planCacheShapeHash`.
- `queries.sampleLine` or the terminal `sample_line` column is the best way to correlate a row back to the raw log.
- auth mechanism commands such as `saslStart` are excluded and should not appear as query operations.
- oversized logical lines above `16 MiB` are skipped and counted as malformed.
- if some inputs fail but at least one succeeds, the tool reports partial input.

## Interpretation Hints

Use these shortcuts when helping a user read the reports:
- In `timeline`, columns after `start` and `total` are the selected group values for `--group-by`.
- In `queries`, `label` is `operation:namespace#shapeId-or-null`.
- In `queries`, `plan` is the dominant rendered plan summary for that shape.
- In `summary`, latency values are milliseconds.
- In `summary`, `ops/min` uses the explicit `--from/--to` range when both are provided; otherwise it uses the observed event range rounded up to whole minutes with a minimum of one minute.

## Working Pattern

When asked to analyze a log:
1. Pick the report that matches the user question.
2. Run the tool against the file or directory the user provided.
3. If the output is large, narrow it with `--top`, `--from`, `--to`, `--bucket`, or `--group-by`.
4. Quote the exact command you used when the user asks how you produced the report.
5. If the user asks about a specific row, use `sample_line` to point back to the raw log.
