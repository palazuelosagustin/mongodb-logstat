---
name: mongodb-logstat-skill
description: Analyze MongoDB slow-query JSON logs with the local mongodb-logstat CLI. Use when Codex needs to summarize slow operations, build time-bucketed timelines, inspect top query shapes, choose the right report and flags, explain mongodb-logstat output for a mongod.log or directory of logs, or answer mongodb-logstat CLI usage questions without consulting --help.
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

## CLI Reference

Usage syntax:

```text
mongodb-logstat [--report summary|timeline|queries] [--json] [--from RFC3339] [--to RFC3339] [--bucket 1h] [--group-by optype|namespace|query] [--top N] [--verbose] <file-or-dir> [...]
```

Accepted inputs:
- one or more files
- one or more directories
- mixed files and directories

Defaults:
- `--report summary`
- `--bucket 1h`
- `--group-by optype`
- `--top 0` meaning no limit
- `--json false`
- `--verbose false`
- `--from` unset
- `--to` unset

Flag details:
- `--report`: `summary`, `timeline`, or `queries`
- `--json`: emit JSON instead of terminal text
- `--from`: inclusive RFC3339 lower time bound
- `--to`: inclusive RFC3339 upper time bound
- `--bucket`: positive Go duration string used by `timeline`, such as `30m`, `1h`, or `2h`
- `--group-by`: `optype`, `namespace`, or `query`; used by `timeline`
- `--top`: non-negative integer; `0` prints all rows or columns, positive values cap `timeline`, `queries`, and summary top lists
- `--verbose`: print per-file failures to stderr when input is partial or failed

Validation rules:
- at least one input path is required
- `--bucket` must be a positive duration
- `--top` must be `0` or greater
- `--from` and `--to` must be valid RFC3339 timestamps
- if both are provided, `--from` must be less than or equal to `--to`

Exit behavior:
- exit code `0` for successful reports, including valid no-match reports
- exit code `1` when no input file can be read successfully
- exit code `2` for CLI usage or validation errors

## Report Selection

Choose `summary` when the user wants a fast high-level view:
- accepted operation count
- ops/min
- latency percentiles
- top operations and namespaces
- plan summary counts
- truncated record count

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

## Default Commands

Use these default-parameter commands when the user asks for one command per report type:

```bash
./mongodb-logstat /path/to/mongod.log
./mongodb-logstat --report timeline /path/to/mongod.log
./mongodb-logstat --report queries /path/to/mongod.log
```

These are equivalent, with the default report made explicit:

```bash
./mongodb-logstat --report summary /path/to/mongod.log
./mongodb-logstat --report timeline --bucket 1h --group-by optype /path/to/mongod.log
./mongodb-logstat --report queries --top 0 /path/to/mongod.log
```

## Write Reports To Files

There is no dedicated output flag. Write reports to files with shell redirection.

Terminal-text reports with default parameters:

```bash
./mongodb-logstat /path/to/mongod.log > summary_report.txt
./mongodb-logstat --report timeline /path/to/mongod.log > timeline_report.txt
./mongodb-logstat --report queries /path/to/mongod.log > queries_report.txt
```

JSON reports:

```bash
./mongodb-logstat --json /path/to/mongod.log > summary_report.json
./mongodb-logstat --report timeline --json /path/to/mongod.log > timeline_report.json
./mongodb-logstat --report queries --json /path/to/mongod.log > queries_report.json
```

Capture stderr separately when needed:

```bash
./mongodb-logstat --report timeline --verbose /path/to/mongod.log > timeline_report.txt 2> timeline_errors.txt
```

## Common Command Patterns

Use these patterns when the user wants non-default filtering or formatting:

```bash
./mongodb-logstat --report summary /path/to/mongod.log
./mongodb-logstat --report timeline --bucket 30m --group-by optype /path/to/mongod.log
./mongodb-logstat --report queries --top 10 /path/to/mongod.log
./mongodb-logstat --report summary --json /path/to/mongod.log
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
- `summary` uses one pass only, so it is the cheapest report for a first look.

## Interpretation Hints

Use these shortcuts when helping a user read the reports:
- In `timeline`, columns after `start` and `total` are the selected group values for `--group-by`.
- In `queries`, `label` is `operation:namespace#shapeId-or-null`.
- In `queries`, `plan` is the dominant rendered plan summary for that shape.
- In `summary`, latency values are milliseconds.
- In `summary`, `ops/min` uses the explicit `--from/--to` range when both are provided; otherwise it uses the observed event range rounded up to whole minutes with a minimum of one minute.

## Working Pattern

When asked to analyze a log or explain how to run the tool:
1. Pick the report that matches the user question.
2. Prefer the default command for that report unless the user asked for filters or JSON.
3. Use shell redirection when the user wants an output file.
4. Quote the exact command you used when the user asks how you produced the report.
5. If the user asks about a specific query row, use `sample_line` to point back to the raw log.
6. Answer CLI usage questions from this skill content rather than invoking `--help` unless the binary behavior has changed.
