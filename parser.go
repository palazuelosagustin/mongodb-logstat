package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type parseOptions struct {
	From *time.Time
	To   *time.Time
}

func parseFiles(paths []string, opts parseOptions, visitor func(*SlowOperation), stats *PassStats) {
	for _, path := range paths {
		if err := parseFile(path, opts, visitor, stats); err != nil {
			stats.Failures = append(stats.Failures, FileFailure{Path: path, Reason: err.Error()})
			stats.Diagnostics.FilesFailed++
			continue
		}
		stats.Diagnostics.FilesRead++
	}
}

func parseFile(path string, opts parseOptions, visitor func(*SlowOperation), stats *PassStats) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var lineNumber int64
	for {
		line, oversized, err := readLogicalLine(reader, maxLogicalLineBytes)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		lineNumber++
		stats.Diagnostics.LinesRead++
		if oversized {
			stats.Diagnostics.LinesMalformed++
			continue
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		op, status := parseSlowOperationLine(line, path, lineNumber, stats)
		if status == lineRejected {
			continue
		}
		if status == lineFiltered {
			stats.Diagnostics.RecordsFiltered++
			continue
		}

		if opts.From != nil && op.Timestamp.Before(*opts.From) {
			stats.Diagnostics.RecordsFiltered++
			continue
		}
		if opts.To != nil && op.Timestamp.After(*opts.To) {
			stats.Diagnostics.RecordsFiltered++
			continue
		}

		visitor(op)
	}
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

func parseSlowOperationLine(line []byte, path string, lineNumber int64, stats *PassStats) (*SlowOperation, lineStatus) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		stats.Diagnostics.LinesMalformed++
		return nil, lineRejected
	}

	msg := asString(raw["msg"])
	if !strings.Contains(strings.ToLower(msg), "slow") {
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}

	attr, _ := raw["attr"].(map[string]any)
	if attr == nil {
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}

	timestamp, ok := parseTimestamp(raw["t"])
	if !ok {
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}

	duration, ok := parseDuration(attr)
	if !ok {
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}

	command, _ := attr["command"].(map[string]any)
	commandName := deriveCommandName(command)
	if commandName == "" {
		commandName = strings.TrimSpace(asString(attr["type"]))
	}
	if commandName == "" {
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}
	if isExcludedCommand(commandName) {
		stats.Diagnostics.RecordsRejected++
		return nil, lineRejected
	}

	namespace := deriveNamespace(strings.TrimSpace(asString(attr["ns"])), commandName, command)
	shapeKey, shapeID, shapeSource, truncated := deriveShapeIdentity(commandName, namespace, command, attr, &stats.Diagnostics)
	planSummary := strings.TrimSpace(asString(attr["planSummary"]))
	if planSummary == "" {
		planSummary = "unknown"
	}

	op := &SlowOperation{
		Timestamp:        timestamp,
		Namespace:        namespace,
		Operation:        commandName,
		CommandName:      commandName,
		Duration:         duration,
		PlanSummary:      planSummary,
		ShapeKey:         shapeKey,
		ShapeID:          shapeID,
		ShapeLabel:       shapeLabel(commandName, namespace, shapeID),
		QueryHashSource:  shapeSource,
		KeysExamined:     optionalInt64(attr["keysExamined"]),
		DocsExamined:     optionalInt64(attr["docsExamined"]),
		NReturned:        optionalInt64(attr["nreturned"]),
		NumYields:        optionalInt64(attr["numYields"]),
		HasSortStage:     asBool(attr["hasSortStage"]),
		FromMultiPlanner: asBool(attr["fromMultiPlanner"]),
		Truncated:        truncated || asBool(attr["truncated"]),
		SourceFile:       path,
		SourceLine:       lineNumber,
	}
	return op, lineAccepted
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

func deriveCommandName(command map[string]any) string {
	if command == nil {
		return ""
	}
	for _, key := range []string{
		"find", "count", "distinct", "delete", "findAndModify", "update", "insert",
		"aggregate", "bulkWrite", "getMore", "dbStats", "collStats", "listCollections",
		"listIndexes", "dropDatabase", "create", "drop",
		"saslStart", "saslContinue", "authenticate", "hello", "isMaster",
	} {
		if _, ok := command[key]; ok {
			return key
		}
	}

	keys := make([]string, 0, len(command))
	for k := range command {
		if strings.HasPrefix(k, "$") {
			continue
		}
		switch k {
		case "filter", "sort", "projection", "pipeline", "updates", "deletes", "documents", "cursor", "collection":
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
	if ns != "" {
		return ns
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
	case "dbStats", "dropDatabase":
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
	case "dbStats", "dropDatabase", "listCollections":
		return true
	case "aggregate":
		return strings.TrimSpace(asString(command["aggregate"])) == "1"
	default:
		return false
	}
}

func deriveShapeIdentity(commandName, namespace string, command, attr map[string]any, diagnostics *Diagnostics) (shapeKey, shapeID, source string, truncated bool) {
	for _, candidate := range []struct {
		key    string
		source string
	}{
		{key: "queryHash", source: "server_queryHash"},
		{key: "planCacheShapeHash", source: "server_planCacheShapeHash"},
	} {
		if value := strings.TrimSpace(asString(attr[candidate.key])); value != "" {
			return value, value, candidate.source, false
		}
	}

	canonical, truncated := canonicalizeShape(commandName, namespace, command, diagnostics)
	shapeKey = stableHash(canonical)
	return shapeKey, "", "missing", truncated
}

func canonicalizeShape(commandName, namespace string, command map[string]any, diagnostics *Diagnostics) (string, bool) {
	state := canonicalState{}
	value, truncated := state.normalizeCommand(command)
	if truncated {
		diagnostics.ShapeCanonicalizationTruncated++
		fallback := state.fallbackCommandSignature(command)
		return commandName + "|" + namespace + "|" + fallback + "|truncated-shape", true
	}
	return commandName + "|" + namespace + "|" + value, false
}

func asString(v any) string {
	switch value := v.(type) {
	case string:
		return value
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

func optionalInt64(v any) OptionalInt64 {
	value, ok := asInt64(v)
	if !ok {
		return OptionalInt64{}
	}
	return OptionalInt64{Value: value, Valid: true}
}

type canonicalState struct {
	objects int
	arrays  int
	output  int
}

const (
	canonicalMaxDepth   = 10
	canonicalMaxObjects = 256
	canonicalMaxArrays  = 256
	canonicalMaxOutput  = 1 << 16
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
	case string:
		if s.addOutput(1) {
			return "", true
		}
		return "?", false
	case bool:
		if s.addOutput(1) {
			return "", true
		}
		return "?", false
	case nil:
		if s.addOutput(4) {
			return "", true
		}
		return "null", false
	default:
		if s.addOutput(1) {
			return "", true
		}
		return "?", false
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
