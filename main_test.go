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
	for i := 0; i < 12; i++ {
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
	if op.QueryHashSource != "missing" {
		t.Fatalf("source=%q want missing", op.QueryHashSource)
	}
	if op.ShapeID != "" {
		t.Fatalf("shapeId=%q want empty", op.ShapeID)
	}
	if !op.Truncated {
		t.Fatalf("truncated=%t want true", op.Truncated)
	}
	if stats.Diagnostics.ShapeCanonicalizationTruncated != 1 {
		t.Fatalf("truncatedCount=%d want 1", stats.Diagnostics.ShapeCanonicalizationTruncated)
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
	if row.ShapeID != nil {
		t.Fatalf("shapeId=%v want nil", *row.ShapeID)
	}
	if row.Label != "find:shop.orders#null" {
		t.Fatalf("label=%q want find:shop.orders#null", row.Label)
	}
	if row.SampleLine != "mongo.log:1" {
		t.Fatalf("sampleLine=%q want mongo.log:1", row.SampleLine)
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

func parseLineForTest(t *testing.T, line string) (*SlowOperation, lineStatus) {
	t.Helper()
	stats := &PassStats{}
	return parseSlowOperationLine([]byte(line), "test.log", 1, stats)
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
