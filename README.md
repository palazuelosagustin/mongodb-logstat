# mongodb-logstat

`mongodb-logstat` is a small Go CLI for summarizing MongoDB slow query log output.

It currently supports three report types:

- `summary`
- `timeline`
- `queries`

## Requirements

- Go `1.22+`

## Run

From the project directory:

```bash
env GOTOOLCHAIN=local /usr/bin/go run . --report summary /path/to/mongod.log
```

## Usage

```text
mongodb-logstat [--report summary|timeline|queries] [--json] [--from RFC3339] [--to RFC3339] [--bucket 1h] [--group-by optype|namespace|query] [--top N] [--verbose] <file-or-dir> [...]
```

## Reports

### `summary`

One-pass aggregate report with:

- accepted operation count
- operations per minute
- latency stats
- top operations
- top namespaces
- plan summary counts
- truncated record count

### `timeline`

Two-pass bucketed report for terminal or JSON output.

Useful flags:

- `--bucket 30m`
- `--group-by optype`
- `--group-by namespace`
- `--group-by query`
- `--top N` to limit timeline columns or query rows; omit it to print all

Example:

```bash
env GOTOOLCHAIN=local /usr/bin/go run . --report timeline --bucket 30m --top 10 /home/agustin.palazuelos/replSet/rs0/rs1/mongod.log
```

### `queries`

Two-pass top-N per-query-shape report with exact counts for selected shapes and latency stats for those shapes.

## JSON Output

Pass `--json` to emit structured JSON. All JSON reports include:

- `schemaVersion`
- `report`
- `range`
- `observedRange`
- `diagnostics`

## Notes

- Oversized logical lines above `16 MiB` are skipped and counted as malformed.
- Partial input is reported in terminal output and in JSON diagnostics.
- If no input file can be read successfully, the program exits with code `1`.
- Valid no-match reports still exit with code `0`.

## Test

```bash
env GOTOOLCHAIN=local /usr/bin/go test ./...
```
