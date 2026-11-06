# Changelog

All notable changes are listed here. Versions follow
[semantic versioning](https://semver.org/); see "Versions and releases" in the
README.

## Unreleased

## v0.1.0 - 2026-11-06

First versioned release. It contains everything built so far, for MongoDB
5.0+ structured JSON logs (plain, gzip, Kubernetes, `kubectl`/`docker` and
syslog/journald prefixes):

- Reports: `timeline`, `queries`, `clients`, `analysis` (bounded, redacted
  JSON for AI-assisted investigation), `events`, `messages` and
  `connections`.
- Per-node attribution from startup lines: a Nodes table, `nodes[]` in JSON,
  and `--group-by node`.
- Copies of the same log (for example rotated and live files) are read once;
  non-log files in directories are skipped and counted.
- Claude and Codex agent skills.
- `--version`, and `toolVersion` in every JSON output.
