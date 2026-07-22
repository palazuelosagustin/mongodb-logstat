package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOversizedLineDoesNotStopParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	valid1 := slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)
	valid2 := slowLine("2026-07-17T10:01:00Z", "find", "shop", "orders", 200, nil)
	oversized := strings.Repeat("x", maxLogicalLineBytes+10)
	content := valid1 + "\n" + oversized + "\n" + valid2 + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	report := computeSummary([]string{path}, nil, Config{Top: 10})
	if report.Accepted != 2 {
		t.Fatalf("accepted=%d want 2", report.Accepted)
	}
	if report.Stats.Diagnostics.LinesMalformed != 1 {
		t.Fatalf("malformed=%d want 1", report.Stats.Diagnostics.LinesMalformed)
	}
}

func TestAuthMechanismCommandsAreRejected(t *testing.T) {
	line := slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
		"type": "command",
		"command": map[string]any{
			"saslStart": 1,
			"mechanism": "SCRAM-SHA-256",
			"$db":       "admin",
		},
		"durationMillis": 100,
	})
	stats := &PassStats{}
	op, status := parseSlowOperationLine([]byte(line), "test.log", 1, stats)
	if status != lineRejected {
		t.Fatalf("status=%v want %v", status, lineRejected)
	}
	if op != nil {
		t.Fatalf("op=%+v want nil", op)
	}
	if stats.Diagnostics.RecordsRejected != 1 {
		t.Fatalf("recordsRejected=%d want 1", stats.Diagnostics.RecordsRejected)
	}
}

func TestOptionalMetricsPreserveMissingVsZero(t *testing.T) {
	withZero, status := parseLineForTest(t, slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, map[string]any{
		"keysExamined": 0,
	}))
	if status != lineAccepted {
		t.Fatalf("status=%v", status)
	}
	if !withZero.KeysExamined.Valid || withZero.KeysExamined.Value != 0 {
		t.Fatalf("expected valid zero metric: %+v", withZero.KeysExamined)
	}

	missing, status := parseLineForTest(t, slowLine("2026-07-17T10:01:00Z", "find", "shop", "orders", 100, nil))
	if status != lineAccepted {
		t.Fatalf("status=%v", status)
	}
	if missing.KeysExamined.Valid {
		t.Fatalf("expected missing metric to be invalid: %+v", missing.KeysExamined)
	}
}

func TestNamespaceDerivationRules(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "aggregate one is db scoped",
			line: slowLine("2026-07-17T10:00:00Z", "aggregate", "shop", "1", 100, nil),
			want: "db.shop",
		},
		{
			name: "getMore collection fallback",
			line: slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
				"command":        map[string]any{"getMore": 123, "collection": "orders", "$db": "shop"},
				"durationMillis": 100,
			}),
			want: "shop.orders",
		},
		{
			name: "bulkWrite collection field",
			line: slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
				"command":        map[string]any{"bulkWrite": "orders", "$db": "shop"},
				"durationMillis": 100,
			}),
			want: "shop.orders",
		},
		{
			name: "database scoped fallback",
			line: slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
				"command":        map[string]any{"dbStats": 1, "$db": "shop"},
				"durationMillis": 100,
			}),
			want: "db.shop",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, status := parseLineForTest(t, tc.line)
			if status != lineAccepted {
				t.Fatalf("status=%v", status)
			}
			if op.Namespace != tc.want {
				t.Fatalf("namespace=%q want %q", op.Namespace, tc.want)
			}
		})
	}
}

func TestPartialInputDiagnosticsAndTerminalHeader(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "good.log")
	if err := os.WriteFile(validPath, []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(dir, "missing.log")

	reportText, _, err := Execute(Config{
		Report: "summary",
		Top:    10,
		Inputs: []string{validPath, missingPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reportText, "Input status: partial (1/2 files read successfully)") {
		t.Fatalf("missing partial header:\n%s", reportText)
	}

	reportJSON, _, err := Execute(Config{
		Report: "summary",
		Top:    10,
		JSON:   true,
		Inputs: []string{validPath, missingPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(reportJSON), &body); err != nil {
		t.Fatal(err)
	}
	diagnostics := body["diagnostics"].(map[string]any)
	if diagnostics["inputStatus"] != "partial" {
		t.Fatalf("inputStatus=%v want partial", diagnostics["inputStatus"])
	}
}

func TestNoMatchJSONContracts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	if err := os.WriteFile(path, []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	from := mustTime(t, "2026-07-17T11:00:00Z")
	to := mustTime(t, "2026-07-17T12:00:00Z")

	for _, report := range []string{"summary", "timeline", "queries"} {
		t.Run(report, func(t *testing.T) {
			out, _, err := Execute(Config{
				Report:  report,
				JSON:    true,
				Top:     10,
				From:    &from,
				To:      &to,
				Inputs:  []string{path},
				Bucket:  time.Hour,
				GroupBy: "optype",
			})
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(out), &body); err != nil {
				t.Fatal(err)
			}
			if int(body["schemaVersion"].(float64)) != 1 {
				t.Fatalf("schemaVersion=%v", body["schemaVersion"])
			}
			switch report {
			case "summary":
				latency := body["latencyMs"].(map[string]any)
				if latency["min"] != nil || latency["avg"] != nil {
					t.Fatalf("expected null latency fields: %+v", latency)
				}
			case "timeline":
				rows := body["rows"].([]any)
				if len(rows) != 0 {
					t.Fatalf("rows=%d want 0", len(rows))
				}
			case "queries":
				rows := body["rows"].([]any)
				if len(rows) != 0 {
					t.Fatalf("rows=%d want 0", len(rows))
				}
			}
		})
	}
}

func TestTruncatedCanonicalizationFallback(t *testing.T) {
	deep := map[string]any{}
	current := deep
	for i := 0; i < 40; i++ {
		next := map[string]any{}
		current["x"] = next
		current = next
	}
	line := slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
		"command":        map[string]any{"find": "orders", "$db": "shop", "filter": deep},
		"durationMillis": 100,
	})
	stats := &PassStats{}
	op, status := parseSlowOperationLine([]byte(line), "test.log", 1, stats)
	if status != lineAccepted {
		t.Fatalf("status=%v", status)
	}
	if op.QueryHashSource != "fallback_truncated" {
		t.Fatalf("source=%q want fallback_truncated", op.QueryHashSource)
	}
	if op.ShapeID == "" {
		t.Fatalf("shapeId should be a deterministic fallback hash")
	}
	if !op.Truncated {
		t.Fatalf("truncated=%t want true", op.Truncated)
	}
	if stats.Diagnostics.ShapeCanonicalizationTruncated < 1 {
		t.Fatalf("truncatedCount=%d want at least 1", stats.Diagnostics.ShapeCanonicalizationTruncated)
	}
}

func TestTimelineWithoutTopShowsAllOperationColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
			"type":           "command",
			"command":        map[string]any{"find": "orders", "$db": "shop"},
			"durationMillis": 100,
		}),
		slowLineCustom("2026-07-17T10:01:00Z", map[string]any{
			"type":           "command",
			"command":        map[string]any{"getMore": 123, "collection": "orders", "$db": "shop"},
			"durationMillis": 100,
		}),
		slowLineCustom("2026-07-17T10:02:00Z", map[string]any{
			"type":           "command",
			"command":        map[string]any{"update": "orders", "$db": "shop"},
			"durationMillis": 100,
		}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := computeTimeline([]string{path}, nil, Config{Bucket: 30 * time.Minute, GroupBy: "optype", Top: 0})
	if containsString(report.Columns, "other") {
		t.Fatalf("columns=%v did not expect other", report.Columns)
	}
	for _, want := range []string{"find", "getMore", "update"} {
		if !containsString(report.Columns, want) {
			t.Fatalf("columns=%v missing %s", report.Columns, want)
		}
	}
}

func TestQueriesTwoPassTopNAndLatency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLineWithFilter("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, map[string]any{"a": 1}),
		slowLineWithFilter("2026-07-17T10:01:00Z", "find", "shop", "orders", 500, map[string]any{"a": 2}),
		slowLineWithFilter("2026-07-17T10:02:00Z", "find", "shop", "orders", 900, map[string]any{"a": 3}),
		slowLineWithFilter("2026-07-17T10:03:00Z", "find", "shop", "orders", 50, map[string]any{"b": 1}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := computeQueries([]string{path}, nil, Config{Top: 1})
	if len(report.Rows) != 1 {
		t.Fatalf("rows=%d want 1", len(report.Rows))
	}
	row := report.Rows[0]
	if row.ShapeID == nil || *row.ShapeID == "" {
		t.Fatalf("shapeId should be a deterministic local hash")
	}
	if strings.Contains(row.Label, "#null") || !strings.HasPrefix(row.Label, "find:shop.orders#") {
		t.Fatalf("label=%q should be a non-null find shape label", row.Label)
	}
	if row.SampleLine != "mongo.log:3" {
		t.Fatalf("sampleLine=%q want worst sample mongo.log:3", row.SampleLine)
	}
	if row.Count != 3 {
		t.Fatalf("count=%d want 3", row.Count)
	}
	if row.P95Ms != 900 || row.MaxMs != 900 {
		t.Fatalf("p95=%d max=%d want 900/900", row.P95Ms, row.MaxMs)
	}
}

func TestSummaryRateSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil),
		slowLine("2026-07-17T10:00:30Z", "find", "shop", "orders", 100, nil),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := computeSummary([]string{path}, nil, Config{Top: 10})
	if report.OpsPerMinute != 2 {
		t.Fatalf("opsPerMinute=%v want 2", report.OpsPerMinute)
	}

	from := mustTime(t, "2026-07-17T10:00:00Z")
	to := mustTime(t, "2026-07-17T10:10:00Z")
	report = computeSummary([]string{path}, nil, Config{Top: 10, From: &from, To: &to})
	if report.OpsPerMinute != 0.2 {
		t.Fatalf("opsPerMinute=%v want 0.2", report.OpsPerMinute)
	}
}

func TestCommandDocumentCanComeFromTypeNamedAttr(t *testing.T) {
	line := slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
		"type": "collection23mand",
		"ns":   "shop.orders",
		"collection23mand": map[string]any{
			"findAndModify": "orders",
			"$db":           "shop",
			"query":         map[string]any{"_id": 1},
			"update":        map[string]any{"$set": map[string]any{"status": "done"}},
		},
		"durationMillis": 100,
	})
	op, status := parseLineForTest(t, line)
	if status != lineAccepted {
		t.Fatalf("status=%v", status)
	}
	if op.Operation != "findAndModify" || op.Category != "write" {
		t.Fatalf("operation=%q category=%q want findAndModify/write", op.Operation, op.Category)
	}
}

func TestFindAndModifyAliasesCountAsFindAndModify(t *testing.T) {
	line := slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
		"type":           "findandmodify",
		"command":        map[string]any{"findAndModify": "orders", "$db": "shop", "query": map[string]any{"_id": 1}, "update": map[string]any{"$set": map[string]any{"status": "done"}}},
		"durationMillis": 100,
	})
	op, status := parseLineForTest(t, line)
	if status != lineAccepted {
		t.Fatalf("status=%v", status)
	}
	if op.Operation != "findAndModify" || op.Category != "write" {
		t.Fatalf("operation=%q category=%q want findAndModify/write", op.Operation, op.Category)
	}
	if op.Namespace != "shop.orders" {
		t.Fatalf("namespace=%q want shop.orders", op.Namespace)
	}
}

func TestTimelineAndAnalysisCountFindAndModify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
			"type":           "findandmodify",
			"command":        map[string]any{"findAndModify": "orders", "$db": "shop", "query": map[string]any{"_id": 1}, "update": map[string]any{"$set": map[string]any{"status": "done"}}},
			"durationMillis": 100,
		}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	timeline := computeTimeline([]string{path}, nil, Config{Bucket: time.Hour, GroupBy: "optype", Top: 0})
	if !containsString(timeline.Columns, "findAndModify") {
		t.Fatalf("timeline columns=%v missing findAndModify", timeline.Columns)
	}
	if len(timeline.Rows) != 1 || timeline.Rows[0].Counts["findAndModify"] != 1 || timeline.Rows[0].Counts["other"] != 0 {
		t.Fatalf("timeline rows=%+v want one findAndModify and no other", timeline.Rows)
	}

	body, _ := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: time.Hour, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	global := body["global"].(map[string]any)
	ops := global["countsByOperation"].(map[string]any)
	if got := int(ops["findAndModify"].(float64)); got != 1 {
		t.Fatalf("analysis findAndModify count=%d want 1 in %+v", got, ops)
	}
	if value, ok := ops["other"]; ok && int(value.(float64)) != 0 {
		t.Fatalf("analysis other count=%v want 0 in %+v", value, ops)
	}
	shape := body["queryShapes"].([]any)[0].(map[string]any)
	if shape["operation"] != "findAndModify" || shape["category"] != "write" {
		t.Fatalf("analysis shape operation/category=%v/%v want findAndModify/write", shape["operation"], shape["category"])
	}
}

func TestInternalCommandsNormalizeToInternal(t *testing.T) {
	line := slowLineCustom("2026-07-17T10:00:00Z", map[string]any{
		"type":           "configTerm",
		"durationMillis": 166,
	})
	op, status := parseLineForTest(t, line)
	if status != lineAccepted {
		t.Fatalf("status=%v", status)
	}
	if op.Operation != "internal" || op.Category != "internal" || !op.Internal {
		t.Fatalf("operation=%s category=%s internal=%t", op.Operation, op.Category, op.Internal)
	}
}

func TestAnalysisReportSchemaV2NoRawValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	line := slowLineWithFilter("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, map[string]any{"secret": "do-not-leak"})
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := Execute(Config{Report: "analysis", Bucket: 30 * time.Minute, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000, Inputs: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "do-not-leak") {
		t.Fatalf("analysis leaked raw predicate value: %s", out)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if int(body["schemaVersion"].(float64)) != 2 || body["report"] != "analysis" {
		t.Fatalf("unexpected analysis header: %+v", body)
	}
	shapes := body["queryShapes"].([]any)
	if len(shapes) != 1 {
		t.Fatalf("queryShapes=%d want 1", len(shapes))
	}
}

func TestClientsReportCorrelatesByExactCtxAndJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		clientEventLine("2026-07-17T10:00:00Z", "conn1", "Connection accepted", map[string]any{"connectionId": 1, "remote": "10.20.30.41:55100"}),
		clientEventLine("2026-07-17T10:00:01Z", "conn1", "client metadata", map[string]any{"remote": "10.20.30.41:55100", "doc": map[string]any{"application": map[string]any{"name": "checkout"}, "driver": map[string]any{"name": "nodejs", "version": "6.6"}, "os": map[string]any{"type": "Linux", "name": "Ubuntu", "version": "24.04"}}}),
		clientEventLine("2026-07-17T10:00:02Z", "conn1", "Authentication succeeded", map[string]any{"principalName": "appUser", "authenticationDatabase": "admin", "mechanism": "SCRAM-SHA-256", "remote": "10.20.30.41:55100"}),
		clientEventLine("2026-07-17T10:00:03Z", "conn2", "Connection accepted", map[string]any{"connectionId": 2, "remote": "10.20.30.42:55101"}),
		clientEventLine("2026-07-17T10:00:04Z", "conn2", "Authentication succeeded", map[string]any{"principalName": "appUser", "authenticationDatabase": "admin", "remote": "10.20.30.42:55101"}),
		clientEventLine("2026-07-17T10:00:05Z", "conn3", "Connection accepted", map[string]any{"connectionId": 3, "remote": "10.20.30.43:55102"}),
		clientEventLine("2026-07-17T10:00:06Z", "conn3", "Authentication succeeded", map[string]any{"principalName": "batch", "authenticationDatabase": "reporting", "remote": "10.20.30.43:55102"}),
		slowLineWithCtx("2026-07-17T10:01:00Z", "conn1", "10.20.30.41:55100", "find", "shop", "orders", 100, map[string]any{"docsExamined": 5, "filter": map[string]any{"secret": "do-not-leak"}}),
		slowLineWithCtx("2026-07-17T10:02:00Z", "conn1", "10.20.30.41:55100", "update", "shop", "orders", 200, map[string]any{"docsExamined": 10}),
		slowLineWithCtx("2026-07-17T10:03:00Z", "conn2", "10.20.30.42:55101", "insert", "shop", "orders", 300, nil),
		slowLineWithCtx("2026-07-17T10:04:00Z", "conn3", "10.20.30.43:55102", "aggregate", "reporting", "events", 900, nil),
		slowLineWithCtx("2026-07-17T10:05:00Z", "conn999", "10.20.30.41:59999", "find", "shop", "orders", 1000, nil),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := Execute(Config{Report: "clients", JSON: true, Top: 2, Inputs: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "do-not-leak") {
		t.Fatalf("clients report leaked raw predicate value: %s", out)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if int(body["schemaVersion"].(float64)) != 1 || body["report"] != "clients" {
		t.Fatalf("unexpected clients envelope: %+v", body)
	}
	if body["ranking"] != "count" {
		t.Fatalf("ranking=%v want count", body["ranking"])
	}
	totals := body["totals"].(map[string]any)
	if got := int(totals["loggedSlowOperations"].(float64)); got != 5 {
		t.Fatalf("loggedSlowOperations=%d want 5", got)
	}
	if got := int(totals["correlatedSlowOperations"].(float64)); got != 4 {
		t.Fatalf("correlatedSlowOperations=%d want 4", got)
	}
	if got := int(totals["uncorrelatedSlowOperations"].(float64)); got != 1 {
		t.Fatalf("uncorrelatedSlowOperations=%d want 1", got)
	}
	omitted := body["omitted"].(map[string]any)
	if got := int(omitted["groups"].(float64)); got != 1 {
		t.Fatalf("omitted.groups=%d want 1", got)
	}
	if got := int(omitted["slowOperations"].(float64)); got != 1 {
		t.Fatalf("omitted.slowOperations=%d want 1", got)
	}
	if got := int(omitted["uncorrelatedSlowOperations"].(float64)); got != 1 {
		t.Fatalf("omitted.uncorrelatedSlowOperations=%d want 1", got)
	}

	rows := body["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["user"] != "appUser" || first["authenticationDatabase"] != "admin" || first["clientIP"] != "10.20.30.41" {
		t.Fatalf("unexpected first row identity: %+v", first)
	}
	if got := int(first["loggedSlowOperations"].(float64)); got != 2 {
		t.Fatalf("first.loggedSlowOperations=%d want 2", got)
	}
	ops := first["countsByOperation"].(map[string]any)
	if int(ops["find"].(float64)) != 1 || int(ops["update"].(float64)) != 1 {
		t.Fatalf("first countsByOperation=%+v want find/update", ops)
	}
	if apps := stringSliceFromAny(first["applications"]); len(apps) != 1 || apps[0] != "checkout" {
		t.Fatalf("applications=%+v want checkout", first["applications"])
	}
	if drivers := stringSliceFromAny(first["drivers"]); len(drivers) != 1 || drivers[0] != "nodejs 6.6" {
		t.Fatalf("drivers=%+v want nodejs 6.6", first["drivers"])
	}
	second := rows[1].(map[string]any)
	if second["user"] != "batch" || second["clientIP"] != "10.20.30.43" {
		t.Fatalf("unexpected second row, top selection should be global by rank then duration: %+v", second)
	}
}

func TestClientsReportTimeFiltersMetadataAndSlowOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		clientEventLine("2026-07-17T10:00:00Z", "connOld", "Connection accepted", map[string]any{"remote": "10.0.0.1:1"}),
		clientEventLine("2026-07-17T10:00:01Z", "connOld", "Authentication succeeded", map[string]any{"principalName": "old", "authenticationDatabase": "admin", "remote": "10.0.0.1:1"}),
		clientEventLine("2026-07-17T10:05:00Z", "connIn", "Connection accepted", map[string]any{"remote": "10.0.0.2:2"}),
		clientEventLine("2026-07-17T10:05:00Z", "connIn", "Authentication succeeded", map[string]any{"principalName": "inside", "authenticationDatabase": "admin", "remote": "10.0.0.2:2"}),
		slowLineWithCtx("2026-07-17T10:10:00Z", "connOld", "10.0.0.1:1", "find", "shop", "orders", 100, nil),
		slowLineWithCtx("2026-07-17T10:59:59Z", "connIn", "10.0.0.2:2", "find", "shop", "orders", 100, nil),
		clientEventLine("2026-07-17T11:00:00Z", "connTo", "Authentication succeeded", map[string]any{"principalName": "to", "authenticationDatabase": "admin", "remote": "10.0.0.3:3"}),
		slowLineWithCtx("2026-07-17T11:00:00Z", "connTo", "10.0.0.3:3", "find", "shop", "orders", 100, nil),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	from := mustTime(t, "2026-07-17T10:05:00Z")
	to := mustTime(t, "2026-07-17T11:00:00Z")
	report := computeClients([]string{path}, nil, Config{Top: 0, From: &from, To: &to})
	if report.Accepted != 2 {
		t.Fatalf("accepted=%d want 2", report.Accepted)
	}
	if report.UncorrelatedSlowOperations != 1 {
		t.Fatalf("uncorrelated=%d want 1 because pre-from metadata is ignored", report.UncorrelatedSlowOperations)
	}
	if len(report.Rows) != 1 || report.Rows[0].User != "inside" || report.Rows[0].ClientIP != "10.0.0.2" {
		t.Fatalf("rows=%+v want only inside@admin 10.0.0.2", report.Rows)
	}
}

func TestClientsTerminalGroupingAndColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		clientEventLine("2026-07-17T10:00:00Z", "conn1", "Connection accepted", map[string]any{"remote": "10.20.30.41:55100"}),
		clientEventLine("2026-07-17T10:00:01Z", "conn1", "Authentication succeeded", map[string]any{"principalName": "appUser", "authenticationDatabase": "admin", "remote": "10.20.30.41:55100"}),
		clientEventLine("2026-07-17T10:00:02Z", "conn2", "Connection accepted", map[string]any{"remote": "10.20.30.42:55101"}),
		clientEventLine("2026-07-17T10:00:03Z", "conn2", "Authentication succeeded", map[string]any{"principalName": "appUser", "authenticationDatabase": "admin", "remote": "10.20.30.42:55101"}),
		clientEventLine("2026-07-17T10:00:04Z", "conn3", "Connection accepted", map[string]any{"remote": "10.20.30.43:55102"}),
		clientEventLine("2026-07-17T10:00:05Z", "conn3", "Authentication succeeded", map[string]any{"principalName": "batch", "authenticationDatabase": "reporting", "remote": "10.20.30.43:55102"}),
		slowLineWithCtx("2026-07-17T10:01:00Z", "conn1", "10.20.30.41:55100", "find", "shop", "orders", 100, nil),
		slowLineWithCtx("2026-07-17T10:02:00Z", "conn1", "10.20.30.41:55100", "update", "shop", "orders", 100, nil),
		slowLineWithCtx("2026-07-17T10:02:30Z", "conn1", "10.20.30.41:55100", "unknownCommand", "shop", "orders", 100, nil),
		slowLineWithCtx("2026-07-17T10:03:00Z", "conn2", "10.20.30.42:55101", "insert", "shop", "orders", 100, nil),
		slowLineWithCtx("2026-07-17T10:04:00Z", "conn3", "10.20.30.43:55102", "aggregate", "reporting", "events", 100, nil),
		slowLineWithCtx("2026-07-17T10:05:00Z", "conn999", "10.20.30.41:59999", "delete", "shop", "orders", 100, nil),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := Execute(Config{Report: "clients", Top: 0, Inputs: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Clients report (ranking count, top all, displayed_groups 3, omitted_groups 0, omitted_slow_ops 1)", "appUser@admin", "batch@reporting", "CLIENT_IP", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("terminal output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "appUser@admin") > strings.Index(out, "batch@reporting") {
		t.Fatalf("user groups not ordered by best global rank:\n%s", out)
	}
	if !strings.Contains(out, "unclassified") || strings.Contains(out, "other") {
		t.Fatalf("terminal output should label fallback bucket as unclassified, not other:\n%s", out)
	}
	for _, col := range []string{"find", "aggregate", "insert", "update"} {
		if !strings.Contains(out, col) {
			t.Fatalf("terminal output missing operation column %q:\n%s", col, out)
		}
	}
	if strings.Contains(out, "delete") {
		t.Fatalf("uncorrelated delete operation should not create a displayed column:\n%s", out)
	}
	if !(strings.Index(out, "find") < strings.Index(out, "aggregate") && strings.Index(out, "aggregate") < strings.Index(out, "insert") && strings.Index(out, "insert") < strings.Index(out, "update")) {
		t.Fatalf("operation columns are not in deterministic order:\n%s", out)
	}
}

func TestParseCLIClientsDefaultSortByCount(t *testing.T) {
	cfg, err := parseCLI([]string{"/tmp/mongo.log", "--report", "clients"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SortBy != "count" {
		t.Fatalf("clients default sortBy=%q want count", cfg.SortBy)
	}
	cfg, err = parseCLI([]string{"/tmp/mongo.log", "--report", "clients", "--sort-by", "p95"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SortBy != "p95" {
		t.Fatalf("explicit clients sortBy=%q want p95", cfg.SortBy)
	}
}

func parseLineForTest(t *testing.T, line string) (*SlowOperation, lineStatus) {
	t.Helper()
	stats := &PassStats{}
	return parseSlowOperationLine([]byte(line), "test.log", 1, stats)
}

func clientEventLine(ts, ctx, msg string, attr map[string]any) string {
	doc := map[string]any{
		"t":    map[string]any{"$date": ts},
		"ctx":  ctx,
		"msg":  msg,
		"attr": attr,
	}
	data, _ := json.Marshal(doc)
	return string(data)
}

func slowLineWithCtx(ts, ctx, remote, commandName, db, collection string, durationMs int, extra map[string]any) string {
	command := map[string]any{commandName: collection, "$db": db}
	attr := map[string]any{
		"type":           "command",
		"ns":             "",
		"remote":         remote,
		"command":        command,
		"durationMillis": durationMs,
		"planSummary":    "IXSCAN",
	}
	for k, v := range extra {
		attr[k] = v
	}
	doc := map[string]any{
		"t":    map[string]any{"$date": ts},
		"ctx":  ctx,
		"msg":  "Slow query",
		"attr": attr,
	}
	data, _ := json.Marshal(doc)
	return string(data)
}

func slowLine(ts, commandName, db, collection string, durationMs int, extra map[string]any) string {
	command := map[string]any{commandName: collection, "$db": db}
	if commandName == "aggregate" && collection == "1" {
		command[commandName] = "1"
	}
	attr := map[string]any{
		"type":           "command",
		"ns":             "",
		"command":        command,
		"durationMillis": durationMs,
		"planSummary":    "IXSCAN",
	}
	for k, v := range extra {
		attr[k] = v
	}
	return slowLineCustom(ts, attr)
}

func slowLineWithFilter(ts, commandName, db, collection string, durationMs int, filter map[string]any) string {
	return slowLineCustom(ts, map[string]any{
		"type": "command",
		"command": map[string]any{
			commandName: collection,
			"$db":       db,
			"filter":    filter,
		},
		"durationMillis": durationMs,
		"planSummary":    "IXSCAN",
	})
}

func slowLineCustom(ts string, attr map[string]any) string {
	doc := map[string]any{
		"t":    map[string]any{"$date": ts},
		"msg":  "Slow query",
		"attr": attr,
	}
	data, _ := json.Marshal(doc)
	return string(data)
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestParseCLIAllowsFlagsAfterInputs(t *testing.T) {
	cfg, err := parseCLI([]string{"/tmp/mongo.log", "--report", "queries", "--top", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Report != "queries" {
		t.Fatalf("report=%q want queries", cfg.Report)
	}
	if cfg.Top != 5 {
		t.Fatalf("top=%d want 5", cfg.Top)
	}
	if len(cfg.Inputs) != 1 || cfg.Inputs[0] != "/tmp/mongo.log" {
		t.Fatalf("inputs=%v want [/tmp/mongo.log]", cfg.Inputs)
	}
}

func TestAnalysisDiagnosticsAccountingAndSchemaFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	nonSlow := `{"t":{"$date":"2026-07-17T10:00:00Z"},"msg":"Connection accepted","attr":{}}`
	accepted := slowLine("2026-07-17T10:01:00Z", "find", "shop", "orders", 100, nil)
	rejected := slowLineCustom("2026-07-17T10:02:00Z", map[string]any{"type": "command", "command": map[string]any{"find": "orders", "$db": "shop"}})
	content := strings.Join([]string{nonSlow, accepted, rejected, "{"}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	body, raw := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: time.Hour, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	if strings.Contains(raw, "countsByNamespace") || strings.Contains(raw, "indicatorCounts") || strings.Contains(raw, `"plans"`) {
		t.Fatalf("analysis emitted removed v2 field: %s", raw)
	}
	d := body["diagnostics"].(map[string]any)
	if got := int64(d["validStructuredLines"].(float64)); got != 3 {
		t.Fatalf("validStructuredLines=%d want 3", got)
	}
	if got := int64(d["nonSlowStructuredLines"].(float64)); got != 1 {
		t.Fatalf("nonSlowStructuredLines=%d want 1", got)
	}
	if got := int64(d["slowQueryLines"].(float64)); got != 2 {
		t.Fatalf("slowQueryLines=%d want 2", got)
	}
	if got := int64(d["acceptedSlowOperations"].(float64)); got != 1 {
		t.Fatalf("acceptedSlowOperations=%d want 1", got)
	}
	if got := int64(d["rejectedSlowOperations"].(float64)); got != 1 {
		t.Fatalf("rejectedSlowOperations=%d want 1", got)
	}
	if got := int64(d["malformedLines"].(float64)); got != 1 {
		t.Fatalf("malformedLines=%d want 1", got)
	}
	reasons := d["rejectedByReason"].(map[string]any)
	if got := int64(reasons["missing_duration"].(float64)); got != 1 {
		t.Fatalf("missing_duration=%d want 1", got)
	}
}

func TestAnalysisRateUsesAnalysisDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil),
		slowLine("2026-07-17T10:10:00Z", "find", "shop", "orders", 100, nil),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: time.Hour, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	ranges := body["ranges"].(map[string]any)
	if got := int64(ranges["analysisDurationSeconds"].(float64)); got != 600 {
		t.Fatalf("analysisDurationSeconds=%d want 600", got)
	}
	rate := body["global"].(map[string]any)["rates"].(map[string]any)["overAnalysisRange"].(map[string]any)
	if got := int64(rate["denominatorSeconds"].(float64)); got != 600 {
		t.Fatalf("denominatorSeconds=%d want 600", got)
	}
	if got := rate["loggedSlowOperationsPerMinute"].(float64); got < 0.199999999 || got > 0.200000001 {
		t.Fatalf("loggedSlowOperationsPerMinute=%v want 0.2", got)
	}
}

func TestAnalysisUTCSerialization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLine("2026-07-17T10:00:00-07:00", "find", "shop", "orders", 100, nil),
		slowLine("2026-07-17T18:00:00+01:00", "find", "shop", "orders", 200, nil),
		slowLine("2026-07-17T17:30:00Z", "find", "shop", "orders", 300, nil),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, raw := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: 30 * time.Minute, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	if strings.Contains(raw, "-07:00") || strings.Contains(raw, "+01:00") {
		t.Fatalf("analysis preserved source timezone offset: %s", raw)
	}
	for _, want := range []string{"2026-07-17T17:00:00Z", "2026-07-17T17:30:00Z"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("analysis missing UTC timestamp %s in %s", want, raw)
		}
	}
}

func TestAnalysisShapeKeysAreGlobalAndReferencesAreEmittedOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLineCustom("2026-07-17T10:00:00Z", map[string]any{"type": "command", "command": map[string]any{"find": "orders", "$db": "shop"}, "durationMillis": 1000, "queryHash": "samehash", "planSummary": "IXSCAN"}),
		slowLineCustom("2026-07-17T10:30:00Z", map[string]any{"type": "command", "command": map[string]any{"find": "orders", "$db": "audit"}, "durationMillis": 900, "queryHash": "samehash", "planSummary": "IXSCAN"}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	all, _ := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: 30 * time.Minute, Top: 0, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	allShapes := all["queryShapes"].([]any)
	if len(allShapes) != 2 {
		t.Fatalf("queryShapes=%d want 2", len(allShapes))
	}
	keys := map[string]bool{}
	for _, item := range allShapes {
		shape := item.(map[string]any)
		identity := shape["identity"].(map[string]any)
		if identity["id"] != "samehash" {
			t.Fatalf("identity.id=%v want samehash", identity["id"])
		}
		keys[shape["shapeKey"].(string)] = true
	}
	if len(keys) != 2 {
		t.Fatalf("shape keys are not globally unique: %+v", allShapes)
	}

	top, _ := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: 30 * time.Minute, Top: 1, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	emitted := map[string]bool{}
	for _, item := range top["queryShapes"].([]any) {
		emitted[item.(map[string]any)["shapeKey"].(string)] = true
	}
	assertAnalysisReferencesResolve(t, top, emitted)
	omitted := top["omitted"].(map[string]any)
	if got := int(omitted["queryShapes"].(float64)); got != 1 {
		t.Fatalf("omitted.queryShapes=%d want 1", got)
	}
}

func TestAnalysisPlanEvidenceSeparatesUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLineWithFilter("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, map[string]any{"a": 1}),
		slowLineCustom("2026-07-17T10:01:00Z", map[string]any{"type": "command", "command": map[string]any{"find": "orders", "$db": "shop", "filter": map[string]any{"a": 2}}, "durationMillis": 100}),
		slowLineCustom("2026-07-17T10:02:00Z", map[string]any{"type": "command", "command": map[string]any{"find": "orders", "$db": "shop", "filter": map[string]any{"a": 3}}, "durationMillis": 100, "planSummary": "COLLSCAN", "planCacheKey": "safePlanKey"}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: time.Hour, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	shape := body["queryShapes"].([]any)[0].(map[string]any)
	evidence := shape["planEvidence"].(map[string]any)
	if got := int(evidence["unknownPlanCount"].(float64)); got != 1 {
		t.Fatalf("unknownPlanCount=%d want 1", got)
	}
	variants := evidence["variants"].([]any)
	if len(variants) != 2 {
		t.Fatalf("variants=%d want 2: %+v", len(variants), variants)
	}
	indicators := stringSliceFromAny(shape["indicators"])
	if !containsString(indicators, "multiple_plan_variants_observed") {
		t.Fatalf("missing multiple_plan_variants_observed in %+v", indicators)
	}
}

func TestAnalysisGetMoreOriginatingSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mongo.log")
	lines := []string{
		slowLineCustom("2026-07-17T10:00:00Z", map[string]any{"type": "command", "command": map[string]any{"find": "orders", "$db": "shop"}, "durationMillis": 100, "planSummary": "IXSCAN"}),
		slowLineCustom("2026-07-17T10:01:00Z", map[string]any{"type": "command", "command": map[string]any{"getMore": 123, "collection": "orders", "$db": "shop"}, "originatingCommand": map[string]any{"find": "orders", "$db": "shop"}, "durationMillis": 200, "planSummary": "IXSCAN"}),
		slowLineCustom("2026-07-17T10:02:00Z", map[string]any{"type": "command", "command": map[string]any{"getMore": 456, "collection": "events", "$db": "audit"}, "durationMillis": 300, "planSummary": "IXSCAN"}),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := runAnalysisForTest(t, path, Config{Report: "analysis", Bucket: time.Hour, Top: 10, SortBy: "total-duration", FillGaps: true, MaxBuckets: 10000})
	var foundAssociated bool
	var foundUnassociated bool
	for _, item := range body["queryShapes"].([]any) {
		shape := item.(map[string]any)
		ops := shape["observedOperations"].(map[string]any)
		switch shape["getMoreAssociation"] {
		case "originatingCommand":
			foundAssociated = true
			if shape["operation"] != "find" || shape["category"] != "read" {
				t.Fatalf("associated shape operation/category=%v/%v want find/read", shape["operation"], shape["category"])
			}
			if int(ops["find"].(float64)) != 1 || int(ops["getMore"].(float64)) != 1 {
				t.Fatalf("associated observedOperations=%+v want find/getMore", ops)
			}
		case "unavailable":
			foundUnassociated = true
			if shape["operation"] != "getMore" || shape["category"] != "cursor" {
				t.Fatalf("unassociated shape operation/category=%v/%v want getMore/cursor", shape["operation"], shape["category"])
			}
		}
	}
	if !foundAssociated || !foundUnassociated {
		t.Fatalf("foundAssociated=%t foundUnassociated=%t", foundAssociated, foundUnassociated)
	}
}

func runAnalysisForTest(t *testing.T, path string, cfg Config) (map[string]any, string) {
	t.Helper()
	cfg.Inputs = []string{path}
	out, _, err := Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	return body, out
}

func assertAnalysisReferencesResolve(t *testing.T, body map[string]any, emitted map[string]bool) {
	t.Helper()
	check := func(keys []any) {
		for _, value := range keys {
			key := value.(string)
			if !emitted[key] {
				t.Fatalf("dangling shape reference %q", key)
			}
		}
	}
	for _, item := range body["timeline"].([]any) {
		check(item.(map[string]any)["dominantShapeKeys"].([]any))
	}
	for _, item := range body["hotWindows"].([]any) {
		check(item.(map[string]any)["dominantShapeKeys"].([]any))
	}
	for _, item := range body["namespaces"].([]any) {
		check(item.(map[string]any)["topShapeKeys"].([]any))
	}
}

func stringSliceFromAny(v any) []string {
	items := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}
