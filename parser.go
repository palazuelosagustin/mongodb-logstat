package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

type parseOptions struct {
	From              *time.Time
	To                *time.Time
	OnStructured      func(time.Time)
	OnStructuredEvent func(StructuredEvent)
}

type StructuredEvent struct {
	Timestamp  time.Time
	Raw        map[string]any
	SourceFile string
	SourceLine int64
}

func parseFiles(paths []string, opts parseOptions, visitor func(*SlowOperation), stats *PassStats) {
	labels := fileLabels(paths)
	// A second copy of a log (mongod.log next to mongod.log.gz, or an earlier
	// snapshot of the same growing file) only contributes the lines past what
	// was already read from the first copy.
	copies := map[string]*copyGroup{}
	for _, path := range paths {
		err := parseOneFile(path, labels[path], copies, true, opts, visitor, stats)
		if err == errNotACopy {
			err = parseOneFile(path, labels[path], copies, false, opts, visitor, stats)
		}
		if err != nil {
			stats.Failures = append(stats.Failures, FileFailure{Path: path, Reason: err.Error()})
			stats.Diagnostics.FilesFailed++
			continue
		}
		stats.Diagnostics.FilesRead++
	}
}

// copyGroup tracks files that start with the same line.
type copyGroup struct {
	label    string   // first file read
	lines    int64    // most lines read from any file in the group
	prevHash uint64   // hash of that file's line lines-1
	lastHash uint64   // hash of that file's last line
	head     []uint64 // hashes of the first file's first copyCheckLines lines
}

// copyCheckLines is how many leading lines must match before a file is
// treated as a copy of an earlier one.
const copyCheckLines = 64

var errNotACopy = errors.New("not a copy")

func lineHash(line []byte) uint64 {
	h := fnv.New64a()
	h.Write(line)
	return h.Sum64()
}

func parseOneFile(path, label string, copies map[string]*copyGroup, dedupe bool, opts parseOptions, visitor func(*SlowOperation), stats *PassStats) error {
	src, err := openLog(path)
	if err != nil {
		return err
	}
	defer src.Close()
	return parseLog(src, label, copies, dedupe, opts, visitor, stats)
}

// parseLog reads one open log. label is the short, unique name used for the
// file in sample references (see fileLabels).
func parseLog(src *logSource, label string, copies map[string]*copyGroup, dedupe bool, opts parseOptions, visitor func(*SlowOperation), stats *PassStats) error {
	switch src.Format {
	case formatLegacy:
		return errLegacyTextLog
	case formatUnknown:
		return errNotMongoLog
	}
	var group *copyGroup
	var skipLines int64
	if fp := src.Fingerprint; fp != "" {
		group = copies[fp]
		if group == nil {
			group = &copyGroup{label: label}
			copies[fp] = group
		} else if dedupe {
			skipLines = group.lines
		}
	}
	reader := bufio.NewReader(src.Reader)
	var lineNumber int64
	var skipped int64
	var prevLine, lastLine []byte
	for {
		line, oversized, err := readLogicalLine(reader, maxLogicalLineBytes)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		lineNumber++
		prevLine, lastLine = lastLine, line
		if group != nil && lineNumber <= copyCheckLines {
			h := lineHash(line)
			switch {
			case lineNumber < skipLines && lineNumber <= int64(len(group.head)) && h != group.head[lineNumber-1]:
				// Same first log line, different content: two different logs.
				return errNotACopy
			case skipLines == 0 && group.lines == 0:
				group.head = append(group.head, h)
			}
		}
		if lineNumber < skipLines {
			if lineNumber == skipLines-1 && lineHash(line) != group.prevHash {
				// Same first line, different content: two different logs.
				return errNotACopy
			}
			skipped++
			continue
		}
		if lineNumber == skipLines && lineHash(line) == group.lastHash {
			// The first copy may end with a partly written line; only an
			// identical line is a duplicate.
			skipped++
			continue
		}
		stats.Diagnostics.LinesRead++
		if oversized {
			stats.Diagnostics.LinesMalformed++
			continue
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if i := structuredStart(line); i > 0 {
			line = line[i:]
		}

		raw, ok := parseStructuredLine(line, stats)
		if !ok {
			continue
		}
		if ts, ok := parseTimestamp(raw["t"]); ok {
			updateObservedRange(&stats.InputObservedFrom, &stats.InputObservedTo, ts)
			if opts.OnStructured != nil {
				opts.OnStructured(ts)
			}
			if opts.OnStructuredEvent != nil && withinTimeRange(ts, opts.From, opts.To) {
				opts.OnStructuredEvent(StructuredEvent{Timestamp: ts, Raw: raw, SourceFile: label, SourceLine: lineNumber})
			}
		}

		op, status := parseSlowOperationDoc(raw, label, lineNumber, stats)
		if status == lineRejected {
			continue
		}
		if status == lineFiltered {
			stats.Diagnostics.RecordsFiltered++
			continue
		}

		if opts.From != nil && op.Timestamp.Before(*opts.From) {
			stats.Diagnostics.RecordsFiltered++
			stats.Diagnostics.FilteredByTime++
			continue
		}
		if opts.To != nil && !op.Timestamp.Before(*opts.To) {
			stats.Diagnostics.RecordsFiltered++
			stats.Diagnostics.FilteredByTime++
			continue
		}

		stats.Diagnostics.AcceptedSlowOperations++
		if op.Truncated {
			stats.Diagnostics.TruncatedSlowOperations++
		}
		visitor(op)
	}
	if skipped > 0 {
		stats.Diagnostics.DuplicateLinesSkipped += skipped
		stats.Duplicates = append(stats.Duplicates, FileDuplicate{Path: label, Of: group.label})
	}
	if group != nil && lineNumber > group.lines {
		group.lines = lineNumber
		group.prevHash = lineHash(prevLine)
		group.lastHash = lineHash(lastLine)
	}
	return nil
}

func readLogicalLine(r *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > maxBytes {
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
				if err == nil || err == bufio.ErrBufferFull {
					continue
				}
				if err == io.EOF {
					return nil, true, nil
				}
				return nil, false, err
			}
			if err == io.EOF {
				return nil, true, nil
			}
			if err != nil {
				return nil, false, err
			}
			return nil, true, nil
		}
		if err == nil {
			return line, false, nil
		}
		if err == io.EOF {
			if len(line) == 0 {
				return nil, false, io.EOF
			}
			return line, false, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return nil, false, err
	}
}

type lineStatus int

const (
	lineAccepted lineStatus = iota
	lineRejected
	lineFiltered
)

func parseStructuredLine(line []byte, stats *PassStats) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		stats.Diagnostics.LinesMalformed++
		return nil, false
	}
	stats.Diagnostics.ValidStructuredLines++
	return raw, true
}

func parseSlowOperationLine(line []byte, path string, lineNumber int64, stats *PassStats) (*SlowOperation, lineStatus) {
	raw, ok := parseStructuredLine(line, stats)
	if !ok {
		return nil, lineRejected
	}
	if ts, ok := parseTimestamp(raw["t"]); ok {
		updateObservedRange(&stats.InputObservedFrom, &stats.InputObservedTo, ts)
	}
	return parseSlowOperationDoc(raw, path, lineNumber, stats)
}

func parseSlowOperationDoc(raw map[string]any, path string, lineNumber int64, stats *PassStats) (*SlowOperation, lineStatus) {
	if !isSlowQueryLine(raw) {
		stats.Diagnostics.NonSlowStructuredLines++
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}
	stats.Diagnostics.SlowQueryLines++

	attr, _ := raw["attr"].(map[string]any)
	if attr == nil {
		rejectSlow(stats, "missing_attr")
		return nil, lineRejected
	}

	timestamp, ok := parseTimestamp(raw["t"])
	if !ok {
		rejectSlow(stats, "missing_timestamp")
		return nil, lineRejected
	}

	duration, ok := parseDuration(attr)
	if !ok {
		rejectSlow(stats, "missing_duration")
		return nil, lineRejected
	}

	command, _ := attr["command"].(map[string]any)
	if command == nil {
		command = commandDocumentFromType(attr)
	}
	originatingCommand, _ := attr["originatingCommand"].(map[string]any)
	commandName := statementCommandName(asString(attr["type"]))
	if commandName == "" {
		commandName = deriveCommandName(command)
	}
	if commandName == "" {
		commandName = strings.TrimSpace(asString(attr["op"]))
	}
	if commandName == "" {
		commandName = strings.TrimSpace(asString(attr["type"]))
	}
	if commandName == "" {
		rejectSlow(stats, "missing_command")
		return nil, lineRejected
	}
	if isExcludedCommand(commandName) {
		rejectSlow(stats, "excluded_command")
		return nil, lineRejected
	}

	info := normalizeOperation(commandName, command, originatingCommand, attr, stats)
	if info.Operation == "" {
		rejectSlow(stats, "unknown_operation")
		return nil, lineRejected
	}
	originOperation := info.Operation
	originCategory := info.Category
	identityCommand := command
	if info.Operation == "getMore" && originatingCommand != nil {
		originName := deriveCommandName(originatingCommand)
		originInfo := normalizeOperation(originName, originatingCommand, nil, attr, stats)
		if originInfo.Operation != "" && originInfo.Operation != "other" && originInfo.Operation != "internal" {
			originOperation = originInfo.Operation
			originCategory = originInfo.Category
			identityCommand = originatingCommand
		}
	}

	namespace := deriveNamespace(strings.TrimSpace(asString(attr["ns"])), info.Operation, command)
	if info.Operation == "getMore" && namespace == "<unknown>" && originatingCommand != nil {
		namespace = deriveNamespace("", originOperation, originatingCommand)
	}

	identity, shapeKey, source, truncated := deriveShapeIdentity(originOperation, namespace, identityCommand, attr, &stats.Diagnostics)
	redacted, redactedTruncated := buildRedactedShape(originOperation, identityCommand, &stats.Diagnostics)
	if redacted.Command == "" {
		redacted.Command = originOperation
	}
	if redactedTruncated {
		truncated = true
	}

	planSummary := sanitizePlanSummary(asString(attr["planSummary"]))
	planCacheKey := sanitizeBoundedString(asString(attr["planCacheKey"]), 128)
	identity.PlanCacheKey = planCacheKey

	lockWait := extractLockWaitMicros(attr["locks"])
	if !lockWait.Valid {
		lockWait = extractLockWaitMicros(attr["lockStats"])
	}

	op := &SlowOperation{
		Timestamp:          timestamp,
		Ctx:                sanitizeBoundedString(asString(raw["ctx"]), 128),
		Remote:             sanitizeBoundedString(asString(attr["remote"]), 256),
		Namespace:          namespace,
		Category:           info.Category,
		Operation:          info.Operation,
		OriginCategory:     originCategory,
		OriginOperation:    originOperation,
		CommandName:        commandName,
		Internal:           info.Internal,
		Duration:           duration,
		DurationMillis:     durationMillis(duration),
		PlanSummary:        planSummary,
		ShapeKey:           shapeKey,
		ShapeID:            identity.ID,
		ShapeLabel:         shapeLabel(originOperation, namespace, identity.ID),
		QueryHashSource:    source,
		ShapeIdentity:      identity,
		RedactedShape:      redacted,
		KeysExamined:       optionalInt64(attr["keysExamined"]),
		DocsExamined:       optionalInt64(attr["docsExamined"]),
		NReturned:          optionalInt64(attr["nreturned"]),
		NMatched:           optionalInt64(attr["nMatched"]),
		NModified:          optionalInt64(attr["nModified"]),
		NInserted:          optionalInt64(attr["ninserted"]),
		NDeleted:           optionalInt64(attr["ndeleted"]),
		NumYields:          optionalInt64(attr["numYields"]),
		BytesRead:          firstOptionalInt64(attr, "bytesRead", "storage.data.bytesRead"),
		BytesWritten:       firstOptionalInt64(attr, "bytesWritten", "storage.data.bytesWritten"),
		TimeReadingMicros:  firstOptionalInt64(attr, "timeReadingMicros", "storage.data.timeReadingMicros"),
		TimeWritingMicros:  firstOptionalInt64(attr, "timeWritingMicros", "storage.data.timeWritingMicros"),
		PlanningTimeMicros: firstOptionalInt64(attr, "planningTimeMicros", "planningTimeMicrosEstimate"),
		LockWaitMicros:     lockWait,
		CPUNanos:           firstOptionalInt64(attr, "cpuNanos"),
		HasSortStage:       optionalBool(attr["hasSortStage"]),
		UsedDisk:           optionalBool(attr["usedDisk"]),
		FromMultiPlanner:   optionalBool(attr["fromMultiPlanner"]),
		Replanned:          optionalBool(attr["replanned"]),
		ReplanReason:       sanitizeReplanReason(asString(attr["replanReason"])),
		PlanCacheKey:       planCacheKey,
		QueryFramework:     sanitizeQueryFramework(asString(attr["queryFramework"])),
		GetMoreAssociated:  info.Operation == "getMore" && originOperation != "getMore",
		Truncated:          truncated || asBool(attr["truncated"]),
		SourceFile:         path,
		SourceLine:         lineNumber,
	}
	if op.Operation == "getMore" && !op.GetMoreAssociated {
		stats.Diagnostics.UnassociatedGetMore++
	}
	return op, lineAccepted
}

// slowQueryLogID is the structured-log id of "Slow query" (MongoDB 4.4+,
// mongod and mongos). Other messages that mention "slow" (for example
// "Slow connection establishment", id 6496400) are not operations.
const slowQueryLogID = 51803

func isSlowQueryLine(raw map[string]any) bool {
	if id, ok := asInt64(raw["id"]); ok {
		return id == slowQueryLogID
	}
	return strings.EqualFold(strings.TrimSpace(asString(raw["msg"])), "slow query")
}

func withinTimeRange(ts time.Time, from, to *time.Time) bool {
	if from != nil && ts.Before(*from) {
		return false
	}
	if to != nil && !ts.Before(*to) {
		return false
	}
	return true
}

func rejectSlow(stats *PassStats, reason string) {
	if reason == "" {
		reason = "unknown"
	}
	stats.Diagnostics.RecordsRejected++
	stats.Diagnostics.RejectedSlowOperations++
	if stats.Diagnostics.RejectedByReason == nil {
		stats.Diagnostics.RejectedByReason = fixedRejectedByReasonMap()
	}
	stats.Diagnostics.RejectedByReason[reason]++
}

func commandDocumentFromType(attr map[string]any) map[string]any {
	typeName := strings.TrimSpace(asString(attr["type"]))
	if typeName == "" {
		return nil
	}
	command, _ := attr[typeName].(map[string]any)
	return command
}

func parseTimestamp(v any) (time.Time, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	date := asString(obj["$date"])
	if date == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, date)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, date)
		if err != nil {
			return time.Time{}, false
		}
	}
	return ts, true
}

func parseDuration(attr map[string]any) (time.Duration, bool) {
	for _, key := range []string{"durationMillis", "durationMillisEstimate"} {
		if ms, ok := asInt64(attr[key]); ok {
			return time.Duration(ms) * time.Millisecond, true
		}
	}
	return 0, false
}

// statementCommandName maps the attr.type of a single-statement slow log line
// to its command. Such lines (6.0+ "type":"update"/"remove" with "command":
// {"q":...,"u":...}) carry the statement, not a command document, so the
// command cannot be read from the document's first key.
func statementCommandName(typeName string) string {
	switch strings.TrimSpace(typeName) {
	case "update":
		return "update"
	case "remove":
		return "delete"
	case "insert":
		return "insert"
	case "query":
		return "find"
	case "getmore":
		return "getMore"
	default:
		return ""
	}
}

// commandOptionKeys are generic command options. The log's command document
// is decoded into a map, so field order (the command name comes first) is
// lost; these keys must not be mistaken for the command name.
var commandOptionKeys = map[string]bool{
	"maxTimeMS": true, "maxTimeMSOpOnly": true, "readConcern": true, "writeConcern": true,
	"readPreference": true, "apiVersion": true, "apiStrict": true, "apiDeprecationErrors": true,
	"clientOperationKey": true, "mayBypassWriteBlocking": true, "autocommit": true,
	"startTransaction": true, "stmtId": true, "stmtIds": true, "ordered": true,
	"bypassDocumentValidation": true, "let": true, "hint": true, "collation": true,
	"allowDiskUse": true, "batchSize": true, "singleBatch": true, "limit": true, "skip": true,
	"nameOnly": true, "authorizedDatabases": true, "term": true, "optimes": true,
	"shardVersion": true, "databaseVersion": true, "allowImplicitCollectionCreation": true,
}

// knownCommandNames are matched before any other key of a command document,
// in this order. Field order is lost when the document is decoded, so a
// known name must win over option keys.
var knownCommandNames = []string{
	"find", "count", "distinct", "delete", "findAndModify", "update", "insert",
	"aggregate", "bulkWrite", "getMore", "commitTransaction", "abortTransaction",
	"dbStats", "collStats", "listCollections", "listIndexes", "listDatabases", "dropDatabase", "create", "drop",
	"createIndexes", "dropIndexes", "collMod", "renameCollection", "validate", "compact", "explain",
	"killCursors", "killOp", "endSessions", "ping", "top", "serverStatus", "currentOp", "buildInfo",
	"getParameter", "setParameter", "shardCollection", "reshardCollection", "moveChunk", "split", "balancerStatus",
	"addShard", "removeShard", "enableSharding", "movePrimary", "refineCollectionShardKey", "listShards",
	"balancerStart", "balancerStop", "features", "fsync", "fsyncUnlock",
	"usersInfo", "rolesInfo", "createUser", "updateUser", "dropUser", "grantRolesToUser", "revokeRolesFromUser",
	"saslStart", "saslContinue", "authenticate", "hello", "isMaster", "replSetHeartbeat", "configTerm",
	"replSetGetStatus", "replSetUpdatePosition", "replSetGetConfig", "replSetReconfig", "replSetStepDown",
}

var knownCommandNamesLower = func() map[string]string {
	m := make(map[string]string, len(knownCommandNames))
	for _, name := range knownCommandNames {
		m[strings.ToLower(name)] = name
	}
	return m
}()

func deriveCommandName(command map[string]any) string {
	if command == nil {
		return ""
	}
	for _, key := range knownCommandNames {
		if _, ok := command[key]; ok {
			return key
		}
	}
	// Older drivers and tools send lowercase names (ismaster, buildinfo).
	for key := range command {
		if name, ok := knownCommandNamesLower[strings.ToLower(key)]; ok {
			return name
		}
	}

	keys := make([]string, 0, len(command))
	for k := range command {
		if strings.HasPrefix(k, "$") {
			continue
		}
		switch k {
		case "filter", "sort", "projection", "pipeline", "updates", "deletes", "documents", "cursor", "collection", "mechanism", "comment", "lsid", "txnNumber":
			continue
		}
		if commandOptionKeys[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		return strings.TrimSpace(key)
	}
	return ""
}

func normalizeOperation(commandName string, command, originatingCommand map[string]any, attr map[string]any, stats *PassStats) OperationInfo {
	name := strings.TrimSpace(commandName)
	if name == "" && command != nil {
		name = deriveCommandName(command)
	}
	name = canonicalCommandName(name)
	switch name {
	case "find", "aggregate", "count", "distinct":
		return OperationInfo{Category: "read", Operation: name}
	case "insert", "update", "delete", "findAndModify", "bulkWrite":
		return OperationInfo{Category: "write", Operation: name}
	case "remove":
		return OperationInfo{Category: "write", Operation: "delete"}
	case "getMore":
		return OperationInfo{Category: "cursor", Operation: "getMore"}
	case "commitTransaction", "abortTransaction":
		return OperationInfo{Category: "transaction", Operation: name}
	case "configTerm", "replSetHeartbeat", "hello", "isMaster":
		return OperationInfo{Category: "internal", Operation: "internal", Internal: true}
	case "dbStats", "collStats", "listCollections", "listIndexes", "listDatabases", "dropDatabase", "create", "drop",
		"createIndexes", "dropIndexes", "collMod", "renameCollection", "validate", "compact", "explain",
		"killCursors", "killOp", "endSessions", "ping", "top", "serverStatus", "currentOp", "buildInfo",
		"getParameter", "setParameter", "shardCollection", "reshardCollection", "moveChunk", "split", "balancerStatus",
		"addShard", "removeShard", "enableSharding", "movePrimary", "refineCollectionShardKey", "listShards",
		"balancerStart", "balancerStop", "features", "fsync", "fsyncUnlock",
		"usersInfo", "rolesInfo", "createUser", "updateUser", "dropUser", "grantRolesToUser", "revokeRolesFromUser":
		return OperationInfo{Category: "command", Operation: "command"}
	case "command":
		if command != nil {
			return normalizeOperation(deriveCommandName(command), command, originatingCommand, attr, stats)
		}
		return OperationInfo{Category: "command", Operation: "command"}
	case "query":
		return OperationInfo{Category: "read", Operation: "find"}
	case "none", "":
		stats.Diagnostics.UnknownOperationTypes++
		return OperationInfo{Category: "other", Operation: "other"}
	default:
		if isInternalCommandName(name) {
			return OperationInfo{Category: "internal", Operation: "internal", Internal: true}
		}
		stats.Diagnostics.UnknownOperationTypes++
		return OperationInfo{Category: "other", Operation: "other"}
	}
}

func canonicalCommandName(name string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "findandmodify", "find_and_modify":
		return "findAndModify"
	case "ismaster":
		return "isMaster"
	default:
		return name
	}
}

func isInternalCommandName(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "repl") || strings.Contains(lower, "heartbeat") || strings.Contains(lower, "config") || strings.HasPrefix(lower, "_")
}

func isExcludedCommand(commandName string) bool {
	switch commandName {
	case "saslStart", "saslContinue", "authenticate":
		return true
	default:
		return false
	}
}

func deriveNamespace(ns, commandName string, command map[string]any) string {
	ns = strings.TrimSpace(ns)
	if strings.HasSuffix(ns, ".$cmd") {
		// 5.0+ logs slow update/delete commands with ns "<db>.$cmd"; the
		// collection is in the command document.
		// A collection starting with "$" (getMore on "$cmd.aggregate", a
		// database-level cursor) has no better namespace than the logged one.
		if derived := deriveNamespace("", commandName, command); derived != "<unknown>" && !strings.HasPrefix(derived, "db.") && !strings.Contains(derived, ".$") {
			return derived
		}
		return ns
	}
	if ns != "" {
		return ns
	}
	if command == nil {
		return "<unknown>"
	}
	db := strings.TrimSpace(asString(command["$db"]))
	if db == "" {
		return "<unknown>"
	}

	collection := ""
	switch commandName {
	case "find", "count", "distinct", "delete", "findAndModify", "update", "insert", "aggregate":
		value := strings.TrimSpace(asString(command[commandName]))
		if value != "" && value != "1" {
			collection = value
		}
	case "bulkWrite":
		collection = strings.TrimSpace(asString(command["bulkWrite"]))
	case "getMore":
		collection = strings.TrimSpace(asString(command["collection"]))
		if collection == "" {
			return "<unknown>"
		}
	case "dbStats", "dropDatabase", "command":
		return "db." + db
	}

	if collection != "" {
		return db + "." + collection
	}
	if isDatabaseScopedCommand(commandName, command) {
		return "db." + db
	}
	return "<unknown>"
}

func isDatabaseScopedCommand(commandName string, command map[string]any) bool {
	switch commandName {
	case "dbStats", "dropDatabase", "listCollections", "command":
		return true
	case "aggregate":
		return strings.TrimSpace(asString(command["aggregate"])) == "1"
	default:
		return false
	}
}

func deriveShapeIdentity(operation, namespace string, command, attr map[string]any, diagnostics *Diagnostics) (ShapeIdentity, string, string, bool) {
	for _, candidate := range []struct {
		key    string
		source string
	}{
		{key: "queryShapeHash", source: "server_queryShapeHash"},
		{key: "planCacheShapeHash", source: "server_planCacheShapeHash"},
		{key: "queryHash", source: "server_queryHash"},
	} {
		if value := sanitizeBoundedString(asString(attr[candidate.key]), 128); value != "" {
			identity := ShapeIdentity{ID: value, Source: candidate.source}
			return identity, analysisShapeKey(namespace, operation, identity), candidate.source, false
		}
	}

	canonical, truncated := canonicalizeShape(operation, namespace, command, diagnostics)
	id := stableHash(canonical)
	source := "local_canonical"
	if truncated {
		source = "fallback_truncated"
		diagnostics.CanonicalizationFailures++
	}
	diagnostics.CanonicalizationFallbacks++
	identity := ShapeIdentity{ID: id, Source: source}
	return identity, analysisShapeKey(namespace, operation, identity), source, truncated
}

func canonicalizeShape(operation, namespace string, command map[string]any, diagnostics *Diagnostics) (string, bool) {
	state := canonicalState{}
	value, truncated := state.normalizeCommand(command)
	if truncated {
		diagnostics.ShapeCanonicalizationTruncated++
		fallback := state.fallbackCommandSignature(command)
		return operation + "|" + namespace + "|" + fallback + "|truncated-shape", true
	}
	return operation + "|" + namespace + "|" + value, false
}

func buildRedactedShape(operation string, command map[string]any, diagnostics *Diagnostics) (RedactedShape, bool) {
	if command == nil {
		return RedactedShape{Command: operation}, false
	}
	shape := RedactedShape{Command: operation}
	var truncated bool
	if v, ok := command["filter"]; ok {
		shape.Filter, truncated = redactValue(v, 0)
	}
	if v, ok := command["q"]; ok && shape.Filter == nil {
		shape.Filter, truncated = redactValue(v, 0)
	}
	if v, ok := command["sort"]; ok {
		shape.Sort, truncated = redactValue(v, 0)
	}
	if v, ok := command["projection"]; ok {
		shape.ProjectionFields = projectionFields(v)
	}
	if v, ok := command["pipeline"]; ok {
		shape.Pipeline, truncated = redactValue(v, 0)
		shape.PipelineStages = pipelineStages(v)
	}
	if v, ok := command["updates"]; ok {
		shape.Update, truncated = redactValue(v, 0)
	}
	if v, ok := command["u"]; ok && shape.Update == nil {
		shape.Update, truncated = redactValue(v, 0)
	}
	if v, ok := command["deletes"]; ok && shape.Filter == nil {
		shape.Filter, truncated = redactValue(v, 0)
	}
	if v, ok := command["hint"]; ok {
		shape.Hint, truncated = redactValue(v, 0)
	}
	if v, ok := command["collation"]; ok {
		shape.Collation, truncated = redactValue(v, 0)
	}
	if v, ok := command["$readPreference"]; ok {
		shape.ReadPreference, truncated = redactValue(v, 0)
	}
	if v, ok := command["readConcern"]; ok {
		shape.ReadConcern, truncated = redactValue(v, 0)
	}
	if v, ok := command["writeConcern"]; ok {
		shape.WriteConcern, truncated = redactValue(v, 0)
	}
	if v, ok := command["allowDiskUse"]; ok {
		b := asBool(v)
		shape.AllowDiskUse = &b
	}
	if truncated {
		diagnostics.ShapeCanonicalizationTruncated++
	}
	return shape, truncated
}

func redactValue(v any, depth int) (any, bool) {
	if depth > 32 {
		return "?truncated", true
	}
	switch value := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			if isVolatileCommandKey(key) {
				continue
			}
			keys = append(keys, sanitizeBoundedString(key, 120))
		}
		sort.Strings(keys)
		out := map[string]any{}
		truncated := false
		if len(keys) > 1000 {
			keys = keys[:1000]
			truncated = true
		}
		for _, key := range keys {
			child, childTruncated := redactValue(value[key], depth+1)
			out[key] = child
			truncated = truncated || childTruncated
		}
		return out, truncated
	case []any:
		if len(value) == 0 {
			return []any{}, false
		}
		if scalarOnlyArray(value) {
			return []any{typeMarker(value[0])}, false
		}
		limit := len(value)
		truncated := false
		if limit > 1000 {
			limit = 1000
			truncated = true
		}
		out := make([]any, 0, limit)
		for i := 0; i < limit; i++ {
			child, childTruncated := redactValue(value[i], depth+1)
			out = append(out, child)
			truncated = truncated || childTruncated
		}
		return out, truncated
	default:
		return typeMarker(v), false
	}
}

func scalarOnlyArray(values []any) bool {
	for _, value := range values {
		switch value.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func typeMarker(v any) string {
	switch value := v.(type) {
	case nil:
		return "?null"
	case bool:
		return "?bool"
	case string:
		if strings.HasPrefix(value, "ObjectId(") {
			return "?objectId"
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return "?date"
		}
		return "?string"
	case json.Number, float64, int, int64:
		return "?number"
	case map[string]any:
		return "?object"
	case []any:
		return "?array"
	default:
		return "?value"
	}
}

func projectionFields(v any) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	fields := make([]string, 0, len(obj))
	for key := range obj {
		fields = append(fields, sanitizeBoundedString(key, 120))
	}
	sort.Strings(fields)
	return fields
}

func pipelineStages(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	stages := make([]string, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, sanitizeBoundedString(key, 80))
		}
		sort.Strings(keys)
		stages = append(stages, keys...)
		if len(stages) >= 100 {
			break
		}
	}
	return stages
}

func asString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func asInt64(v any) (int64, bool) {
	switch value := v.(type) {
	case int:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		return int64(value), true
	case json.Number:
		i, err := value.Int64()
		if err == nil {
			return i, true
		}
		f, err := value.Float64()
		if err != nil {
			return 0, false
		}
		return int64(f), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

func asBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}

func fixedRejectedByReasonMap() map[string]int64 {
	reasons := []string{"missing_attr", "missing_timestamp", "missing_duration", "missing_command", "excluded_command", "unknown_operation", "unknown"}
	out := make(map[string]int64, len(reasons))
	for _, reason := range reasons {
		out[reason] = 0
	}
	return out
}

func sanitizePlanSummary(value string) string {
	value = sanitizeBoundedString(value, 240)
	if value == "" {
		return "unknown"
	}
	allowed := map[string]struct{}{
		"COLLSCAN": {}, "IXSCAN": {}, "EXPRESS_IXSCAN": {}, "IDHACK": {}, "EOF": {},
		"COUNT": {}, "COUNT_SCAN": {}, "DISTINCT_SCAN": {}, "SUBPLAN": {}, "FETCH": {},
		"UPDATE": {}, "DELETE": {}, "INSERT": {}, "BULK_WRITE": {}, "UNKNOWN": {},
	}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return !(r == 95 || r >= 48 && r <= 57 || r >= 65 && r <= 90 || r >= 97 && r <= 122)
	}) {
		upper := strings.ToUpper(token)
		if _, ok := allowed[upper]; ok {
			if upper == "UNKNOWN" {
				return "unknown"
			}
			return upper
		}
	}
	return "?string"
}

func sanitizeQueryFramework(value string) string {
	value = sanitizeBoundedString(value, 40)
	if value == "" {
		return ""
	}
	switch strings.ToLower(value) {
	case "classic", "sbe", "cqf", "bonsai":
		return strings.ToLower(value)
	default:
		return "?string"
	}
}

func sanitizeReplanReason(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "?string"
}

func optionalBool(v any) OptionalBool {
	switch value := v.(type) {
	case bool:
		return OptionalBool{Value: value, Valid: true}
	case string:
		trimmed := strings.TrimSpace(value)
		if strings.EqualFold(trimmed, "true") {
			return OptionalBool{Value: true, Valid: true}
		}
		if strings.EqualFold(trimmed, "false") {
			return OptionalBool{Value: false, Valid: true}
		}
	}
	return OptionalBool{}
}

func optionalInt64(v any) OptionalInt64 {
	value, ok := asInt64(v)
	if !ok {
		return OptionalInt64{}
	}
	return OptionalInt64{Value: value, Valid: true}
}

func firstOptionalInt64(root map[string]any, paths ...string) OptionalInt64 {
	for _, path := range paths {
		if value, ok := valueAtPath(root, path); ok {
			if opt := optionalInt64(value); opt.Valid {
				return opt
			}
		}
	}
	return OptionalInt64{}
}

func valueAtPath(root map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var current any = root
	for _, part := range parts {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func extractLockWaitMicros(v any) OptionalInt64 {
	obj, ok := v.(map[string]any)
	if !ok {
		return OptionalInt64{}
	}
	var total int64
	var found bool
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "timeAcquiringMicros" {
					switch modes := child.(type) {
					case map[string]any:
						for _, modeValue := range modes {
							if n, ok := asInt64(modeValue); ok {
								total += n
								found = true
							}
						}
					default:
						if n, ok := asInt64(child); ok {
							total += n
							found = true
						}
					}
					continue
				}
				walk(child)
			}
		}
	}
	walk(obj)
	if !found {
		return OptionalInt64{}
	}
	return OptionalInt64{Value: total, Valid: true}
}

type canonicalState struct {
	objects int
	arrays  int
	output  int
}

const (
	canonicalMaxDepth   = 32
	canonicalMaxObjects = 1000
	canonicalMaxArrays  = 1000
	canonicalMaxOutput  = 1 << 20
)

func (s *canonicalState) normalizeCommand(command map[string]any) (string, bool) {
	return s.normalizeValue(command, 0)
}

func (s *canonicalState) normalizeValue(v any, depth int) (string, bool) {
	if depth > canonicalMaxDepth {
		return "", true
	}

	switch value := v.(type) {
	case map[string]any:
		s.objects++
		if s.objects > canonicalMaxObjects {
			return "", true
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			if isVolatileCommandKey(key) {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			child, truncated := s.normalizeValue(value[key], depth+1)
			if truncated {
				return "", true
			}
			parts = append(parts, key+":"+child)
		}
		out := "{" + strings.Join(parts, ",") + "}"
		if s.addOutput(len(out)) {
			return "", true
		}
		return out, false
	case []any:
		s.arrays++
		if s.arrays > canonicalMaxArrays {
			return "", true
		}
		if len(value) == 0 {
			if s.addOutput(2) {
				return "", true
			}
			return "[]", false
		}
		if scalarOnlyArray(value) {
			out := "[" + typeMarker(value[0]) + "]"
			if s.addOutput(len(out)) {
				return "", true
			}
			return out, false
		}
		parts := make([]string, 0, len(value))
		for _, item := range value {
			child, truncated := s.normalizeValue(item, depth+1)
			if truncated {
				return "", true
			}
			parts = append(parts, child)
		}
		out := "[" + strings.Join(parts, ",") + "]"
		if s.addOutput(len(out)) {
			return "", true
		}
		return out, false
	case string, bool, json.Number, float64, int, int64, nil:
		marker := typeMarker(value)
		if s.addOutput(len(marker)) {
			return "", true
		}
		return marker, false
	default:
		if s.addOutput(6) {
			return "", true
		}
		return "?value", false
	}
}

func (s *canonicalState) addOutput(n int) bool {
	s.output += n
	return s.output > canonicalMaxOutput
}

func (s *canonicalState) fallbackCommandSignature(command map[string]any) string {
	if command == nil {
		return "{}"
	}
	keys := make([]string, 0, len(command))
	for key := range command {
		if isVolatileCommandKey(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+":"+firstLevelSignature(command[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func firstLevelSignature(v any) string {
	switch value := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			if isVolatileCommandKey(key) {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return "{" + strings.Join(keys, ",") + "}"
	case []any:
		if len(value) == 0 {
			return "[]"
		}
		types := make([]string, 0, len(value))
		seen := map[string]struct{}{}
		for _, item := range value {
			kind := valueKind(item)
			if _, ok := seen[kind]; ok {
				continue
			}
			seen[kind] = struct{}{}
			types = append(types, kind)
		}
		sort.Strings(types)
		return "[" + strings.Join(types, ",") + "]"
	default:
		return valueKind(v)
	}
}

func valueKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "scalar"
	}
}

func isVolatileCommandKey(key string) bool {
	switch key {
	case "lsid", "txnNumber", "$clusterTime", "operationTime", "shardVersion", "databaseVersion", "clientOperationKey", "comment", "signature", "$db":
		return true
	default:
		return false
	}
}
