# mongodb-logstat JSON

Every report takes `--json` (`analysis` is always JSON). Read JSON through
`jq`, never whole: it repeats every key and is several times the size of the
text report.

## Envelope (timeline, queries, clients, events, messages)

| field | meaning |
|---|---|
| `schemaVersion` | `1` |
| `toolVersion` | mongodb-logstat version that wrote it (`v0.1.0`, or `dev (commit)` for a local build) |
| `report` | report name |
| `range` | `from`/`to` requested with `--from/--to` (`null` when not given) |
| `observedRange` | first/last accepted slow operation |
| `diagnostics` | input accounting, below |
| `nodes[]` | one per mongod/mongos: `node`, `host`, `port`, `process`, `replSet`, `clusterRole`, `version`, `files[]`, `from`, `to`, `slowOperations` |

`diagnostics`: `inputStatus` (`complete`/`partial`/`failed`),
`filesDiscovered`, `filesRead`, `filesFailed`, `filesSkipped` (non-log files
in directories), `duplicateLinesSkipped` (lines repeated by a copy of a file
already read), `linesRead`, `linesMalformed`, `validStructuredLines`,
`nonSlowStructuredLines`, `slowQueryLines`, `acceptedSlowOperations`,
`rejectedSlowOperations` with `rejectedByReason` (`excluded_command` is
authentication commands), `recordsFiltered`/`filteredByTime`,
`canonicalizationFallbacks`, `unassociatedGetMore`. Zero counters are left out.

## events

```json
{"kinds": {"rsState": 3, "...": 1},
 "events": [{"time": "2026-10-21T15:14:39.147Z", "node": "rs01-0:27017", "file": "rs01-0/mongod.log",
             "line": 812, "kind": "rsState", "logId": 21358, "detail": "SECONDARY -> PRIMARY",
             "facts": {"oldState": "SECONDARY", "newState": "PRIMARY"}}]}
```

`kind` values and their `facts`: `start` (host, port, pid, dbPath),
`version` (version), `signal` (signal, error), `shutdown` (exitCode),
`uncleanShutdown` (lockFile), `fatal` (error/code when logged), `rsState`
(oldState, newState), `memberState` (hostAndPort, newState), `electionStart`
(electionTimeoutPeriodMillis), `electionWon` (term), `electionLost`
(isDryRun), `syncSource` (oldSyncSource, newSyncSource), `rsConfig` (_id,
version, term, members), `initialSyncStart` (initialSyncAttempt,
initialSyncMaxAttempts), `initialSyncDone` (durationSeconds), `fcv`
(newVersion). Events are sorted by time, then file, then line.

```bash
# state changes of every node, in order
jq -r '.events[] | select(.kind=="rsState" or .kind=="memberState") | [.time,.node,.detail] | @tsv'
# restarts per node
jq -r '[.events[] | select(.kind=="start")] | group_by(.node)[] | [.[0].node, length] | @tsv'
# everything between two times (times compare as strings)
jq -r '.events[] | select(.time >= "2026-11-04T13:00" and .time < "2026-11-04T14:00") | [.time,.node,.kind,.detail] | @tsv'
```

## messages

```json
{"severities": ["F","E","W"], "lines": 68051, "bySeverity": {"W": 68051},
 "groups": 11, "omittedGroups": 0, "omittedLines": 0,
 "messages": [{"severity": "W", "component": "REPL", "logId": 21122,
   "msg": "Oplog fetcher stopped querying remote oplog with error", "count": 18,
   "first": "2026-09-30T05:19:00.764Z", "last": "2026-09-30T10:23:23.241Z",
   "nodes": {"db2:28017": 18}, "files": {"mongod.log": 18},
   "sample": {"file": "mongod.log", "line": 4211, "attr": {"error": "InvalidSyncSource: ..."}}}]}
```

Sorted by severity (F, E, W), then count. `sample` is the first occurrence;
values under `cmd`, `command`, `filter`, `q`, `u`, `pipeline`, `keyValue`,
`doc` and similar keys are type markers (`?string`), strings over 300 bytes
end in `...(truncated)`.

```bash
# errors and fatals only
jq -r '.messages[] | select(.severity != "W") | [.severity,.component,.logId,.count,.first,.msg] | @tsv'
# per-node counts of one message
jq '.messages[] | select(.logId==21122) | .nodes'
# the error text of every sampled warning in a component
jq -r '.messages[] | select(.component=="QUERY") | [.logId, (.sample.attr.error.codeName // .sample.attr.error // "")] | @tsv'
```

## timeline

`groupBy`, `bucket`, `columns[]` (group values, `other` for groups beyond
`--top`), `rows[]` with `start`, `total`, `counts` (per column).

```bash
# busiest buckets
jq -r '.rows | sort_by(-.total)[:5][] | [.start,.total] | @tsv'
# with --group-by node: when did one node have most of the slow operations
jq -r --arg n 'db1:27017' '.rows[] | select((.counts[$n] // 0) > 0.5 * .total) | [.start, .counts[$n], .total] | @tsv'
```

## queries

`rows[]`: `shapeId`, `label` (`op:ns#id`), `namespace`, `operation`, `count`,
`avgMs`, `p95Ms`, `maxMs`, `plan` (dominant plan summary), `sampleLine`
(`file:line` of the worst sample).

## clients

`ranking`, `top`, `totals` (`loggedSlowOperations`,
`correlatedSlowOperations`, `uncorrelatedSlowOperations`,
`cumulativeDurationMs`), `rows[]` per user + client IP: `user`,
`authenticationDatabase`, `clientIP`, `applications[]`, `drivers[]`,
`remotes[]`, `loggedSlowOperations`, `cumulativeDurationMs`, `latencyMs`,
`countsByOperation`, `topNamespaces[]`, `sampleLines[]`; `omitted` for groups
beyond `--top` and the uncorrelated operations.

## connections

`bucket`, `totals` (same fields as a row, without `start`/`node`), `rows[]`
(`start`, `node`, `accepted`, `ended`, `openMax`, `openLast`,
`authSucceeded`, `authFailed`, `refused`; `openMax`/`openLast` are `null`
when the bucket had no accept/end line), `clients[]` (`clientIP`,
`accepted`, `ended`, `authFailed`, `refused`, `applications`, `drivers`,
`nodes`), `omittedClients`, `authFailures[]` (`user`, `db`, `mechanism`,
`error`, `count`, `first`, `last`, `clientIPs`, `nodes`),
`omittedAuthFailures`.

```bash
C="mongodb-logstat --report connections --json --bucket 5m"
# buckets with the most new connections, per node
$C DIR | jq -r '.rows | sort_by(-.accepted)[:10][] | [.start,.node,.accepted,.openMax] | @tsv'
# peak open connections per node
$C DIR | jq -r '.rows | group_by(.node)[] | [.[0].node, (map(.openMax // 0) | max)] | @tsv'
# failures for one user, with the IPs
$C --top 0 DIR | jq -r '.authFailures[] | select(.user=="app") | [.db,.error,.count,(.clientIPs|length)] | @tsv'
```

## analysis (schemaVersion 2)

Top level: `toolVersion`, `semantics`, `ranges` (`requested`, `inputObserved`,
`slowOperationsObserved`, `analysisDurationSeconds`), `options`,
`diagnostics` (`input*` names, `duplicateLinesSkipped`), `nodes[]`,
`global`, `timeline[]`, `hotWindows[]`, `queryShapes[]`, `namespaces[]`,
`internalOperations[]`, `omitted`, `indicatorThresholds`.

- `global`: `loggedSlowOperations`, `cumulativeDurationMs`, `latencyMs`,
  `countsByCategory`, `countsByOperation`, `indicators` (`shapeCounts`,
  `operationCounts` per indicator), `internalCount`, `rates.overAnalysisRange`.
- `timeline[]` (filled buckets): `start`, `end`, `validLogLines`,
  `loggedSlowOperations`, `cumulativeDurationMs`, `latencyP95Ms`,
  `operations`, `dominantShapeKeys`.
- `hotWindows[]` (top 10 buckets by cumulative duration): the bucket fields
  plus `dominantShapes[]` with `shapeKey`, `label`, `cumulativeDurationMs`,
  `durationSharePct` (of the window).
- `queryShapes[]`: `shapeKey` (unique, referenced by the other sections),
  `identity` (`id`, `source`: server `queryShapeHash`/`planCacheShapeHash`/
  `queryHash` or `local_canonical`), `label`, `namespace`, `category`,
  `operation`, `observedOperations`, `redactedShape` (structure with type
  markers, no values), `cursorCounts`, `latencyMs`, `impact`, `execution`
  (averages of docsExamined, keysExamined, nreturned, numYields, cpuNanos,
  ... over records that had them), `storage`, `planner`, `planEvidence`
  (`variants[]`, `unknownPlanCount`), `indicators[]` (names such as `collection_scan_observed`), `ratios`
  (`readDocsPerReturned`, `readKeysPerReturned`, `writeDocsPerMatched`,
  `lockWaitTimeSharePct`, `storageReadTimeSharePct`),
  `worstSample` (`timestamp`, `durationMs`, `sourceFile`, `sourceLine`,
  `planSummary`), `getMoreAssociation`, `truncated`.
- `namespaces[]`: `name`, `loggedSlowOperations`, `cumulativeDurationMs`,
  `shareOfLoggedSlowDurationPct`, `p95Ms`, `maxMs`, `shapeCount`,
  `collectionScanCount`, `docsExamined`, `keysExamined`, `topShapeKeys`.

```bash
A="mongodb-logstat --report analysis --from 2026-11-04T13:00 --to 2026-11-04T15:00 DIR"
# hot windows with their named shapes
$A | jq -r '.hotWindows[] | "\(.start) \(.cumulativeDurationMs)ms", (.dominantShapes[] | "  \(.durationSharePct)% \(.label)")'
# shapes flagged as collection scans, with docs examined per returned
$A | jq -r '.queryShapes[] | select(.indicators | index("collection_scan_observed")) | [.label, .ratios.readDocsPerReturned, .impact.totalDurationMs] | @tsv'
# raw line of the worst sample of the top shape
$A | jq -r '.queryShapes[0].worstSample | "\(.sourceFile):\(.sourceLine)"'
```
