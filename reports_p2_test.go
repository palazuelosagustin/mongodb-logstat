package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func logLine(ts, sev, component string, id int64, msg string, attr map[string]any) string {
	doc := map[string]any{"t": map[string]any{"$date": ts}, "s": sev, "c": component, "id": id, "ctx": "main", "msg": msg, "attr": attr}
	data, _ := json.Marshal(doc)
	return string(data)
}

// squash collapses runs of spaces so checks do not depend on column widths.
func squash(s string) string {
	return regexp.MustCompile(` +`).ReplaceAllString(s, " ")
}

func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	writeFile(t, path, []byte(strings.Join(lines, "\n")+"\n"))
}

func TestEventsReport(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, filepath.Join(dir, "rs0", "mongod.log"),
		logLine("2026-07-17T10:00:00.000Z", "I", "CONTROL", 4615611, "MongoDB starting", map[string]any{"pid": 7, "port": 27017, "dbPath": "/data", "host": "rs0"}),
		logLine("2026-07-17T10:00:00.001Z", "I", "CONTROL", 23403, "Build Info", map[string]any{"buildInfo": map[string]any{"version": "8.0.12-4"}}),
		logLine("2026-07-17T10:00:01.000Z", "I", "REPL", 21358, "Replica set state transition", map[string]any{"newState": "PRIMARY", "oldState": "SECONDARY"}),
		logLine("2026-07-17T10:00:02.000Z", "I", "NETWORK", 22943, "Connection accepted", map[string]any{}),
		logLine("2026-07-17T10:00:03.000Z", "F", "STORAGE", 28595, "Terminating.", map[string]any{"error": "No space left on device"}),
		logLine("2026-07-17T10:00:04.000Z", "I", "CONTROL", 23138, "Shutting down", map[string]any{"exitCode": 14}),
	)
	writeLog(t, filepath.Join(dir, "rs1", "mongod.log"),
		logLine("2026-07-17T10:00:01.500Z", "I", "REPL", 21215, "Member is in new state", map[string]any{"hostAndPort": "rs0:27017", "newState": "PRIMARY"}),
		logLine("2026-07-17T10:00:05.000Z", "I", "REPL", 21088, "Changed sync source", map[string]any{"oldSyncSource": "rs0:27017", "newSyncSource": "rs2:27017"}),
	)

	cfg := Config{Report: "events", Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{dir}}
	out, code, err := Execute(cfg)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	for _, want := range []string{
		"Events report (7 events: ",
		"rs0/mongod.log  start",
		"host rs0 port 27017 pid 7 dbPath /data",
		"version        8.0.12-4",
		"SECONDARY -> PRIMARY",
		"rs1/mongod.log  memberState    rs0:27017 -> PRIMARY",
		"fatal          Terminating.",
		"exitCode 14",
		"syncSource     rs0:27017 -> rs2:27017",
	} {
		if !strings.Contains(squash(out), squash(want)) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Connection accepted") {
		t.Fatalf("connection lines are not events:\n%s", out)
	}
	// Time order across files.
	if strings.Index(out, "rs0:27017 -> PRIMARY") > strings.Index(out, "Terminating.") {
		t.Fatalf("events not in time order:\n%s", out)
	}

	cfg.JSON = true
	out, _, err = Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Report string         `json:"report"`
		Kinds  map[string]int `json:"kinds"`
		Events []struct {
			Time  string         `json:"time"`
			File  string         `json:"file"`
			Line  int64          `json:"line"`
			Kind  string         `json:"kind"`
			LogID int64          `json:"logId"`
			Facts map[string]any `json:"facts"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if body.Report != "events" || len(body.Events) != 7 || body.Kinds["rsState"] != 1 {
		t.Fatalf("unexpected body: %+v", body)
	}
	first := body.Events[0]
	if first.Time != "2026-07-17T10:00:00.000Z" || first.Kind != "start" || first.Line != 1 || first.LogID != 4615611 || first.Facts["host"] != "rs0" {
		t.Fatalf("first event: %+v", first)
	}

	// --from/--to apply to events.
	from := time.Date(2026, 7, 17, 10, 0, 3, 0, time.UTC)
	cfg.From = &from
	out, _, _ = Execute(cfg)
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("events from 10:00:03 = %d want 3", len(body.Events))
	}
}

func TestMessagesReport(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, filepath.Join(dir, "a", "mongod.log"),
		logLine("2026-07-17T10:00:00.000Z", "W", "NETWORK", 23234, "No SSL certificate provided by peer", map[string]any{}),
		logLine("2026-07-17T10:01:00.000Z", "W", "NETWORK", 23234, "No SSL certificate provided by peer", map[string]any{}),
		logLine("2026-07-17T10:02:00.000Z", "W", "QUERY", 23798, "Plan executor error during find command", map[string]any{
			"cmd":   map[string]any{"find": "orders", "filter": map[string]any{"email": "secret@example.com"}},
			"error": map[string]any{"code": 50, "codeName": "MaxTimeMSExpired", "errmsg": "operation exceeded time limit"},
		}),
		logLine("2026-07-17T10:03:00.000Z", "I", "NETWORK", 22943, "Connection accepted", map[string]any{}),
		logLine("2026-07-17T10:04:00.000Z", "E", "REPL", 21122, "Oplog fetcher stopped querying remote oplog with error", map[string]any{"error": strings.Repeat("x", 500)}),
	)
	writeLog(t, filepath.Join(dir, "b", "mongod.log"),
		logLine("2026-07-17T09:59:00.000Z", "W", "NETWORK", 23234, "No SSL certificate provided by peer", map[string]any{}),
	)

	cfg := Config{Report: "messages", Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{dir}}
	out, _, err := Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Messages report (fatal/error/warning: 0 F, 1 E, 4 W lines in 3 groups, top all",
		"E    REPL       21122  1      2026-07-17T10:04:00.000Z",
		"W    NETWORK    23234  3      2026-07-17T09:59:00.000Z  2026-07-17T10:01:00.000Z  2 files",
	} {
		if !strings.Contains(squash(out), squash(want)) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Connection accepted") {
		t.Fatalf("info lines are not messages:\n%s", out)
	}
	// Errors sort before warnings.
	if strings.Index(out, "21122") > strings.Index(out, "23234") {
		t.Fatalf("severity order wrong:\n%s", out)
	}

	cfg.JSON = true
	cfg.Top = 2
	out, _, err = Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "secret@example.com") {
		t.Fatalf("query value leaked into sample:\n%s", out)
	}
	var body struct {
		Lines         int            `json:"lines"`
		BySeverity    map[string]int `json:"bySeverity"`
		Groups        int            `json:"groups"`
		OmittedGroups int            `json:"omittedGroups"`
		OmittedLines  int            `json:"omittedLines"`
		Messages      []MessageRow   `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if body.Lines != 5 || body.Groups != 3 || body.OmittedGroups != 1 || body.OmittedLines != 1 || len(body.Messages) != 2 {
		t.Fatalf("unexpected body: %+v", body)
	}
	ssl := body.Messages[1]
	if ssl.LogID != 23234 || ssl.Files["a/mongod.log"] != 2 || ssl.Files["b/mongod.log"] != 1 || ssl.Sample.File != "b/mongod.log" && ssl.Sample.File != "a/mongod.log" {
		t.Fatalf("ssl row: %+v", ssl)
	}
	fetcher := body.Messages[0]
	errText, _ := fetcher.Sample.Attr.(map[string]any)["error"].(string)
	if !strings.HasSuffix(errText, "...(truncated)") || len(errText) > maxSampleString+20 {
		t.Fatalf("long error not truncated: %q", errText)
	}
}

func TestSampleAttrKeepsErrorsAndRedactsQueries(t *testing.T) {
	got := sampleAttr(map[string]any{
		"cmd":      map[string]any{"filter": map[string]any{"ssn": "123"}},
		"error":    map[string]any{"codeName": "DuplicateKey", "keyValue": map[string]any{"email": "a@b"}},
		"hostPort": "rs1:27017",
	}, 0)
	data, _ := json.Marshal(got)
	s := string(data)
	for _, leak := range []string{"123", "a@b"} {
		if strings.Contains(s, leak) {
			t.Fatalf("leaked %q: %s", leak, s)
		}
	}
	for _, keep := range []string{"DuplicateKey", "rs1:27017"} {
		if !strings.Contains(s, keep) {
			t.Fatalf("lost %q: %s", keep, s)
		}
	}
}
