package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const legacyLog = "2020-01-02T03:04:05.678+0000 I  CONTROL  [initandlisten] MongoDB starting : pid=1 port=27017\n" +
	"2020-01-02T03:04:06.000+0000 I  COMMAND  [conn1] command test.c command: find { find: \"c\" } 150ms\n"

func TestDetectLogFormat(t *testing.T) {
	cases := []struct {
		name string
		head string
		want logFormat
	}{
		{"json", `{"t":{"$date":"2026-07-17T10:00:00Z"},"msg":"x"}`, formatJSON},
		{"json after blank lines", "\n\n  " + `{"t":{"$date":"2026-07-17T10:00:00Z"},"msg":"x"}`, formatJSON},
		{"empty", "", formatJSON},
		{"legacy 4.2", legacyLog, formatLegacy},
		{"legacy with Z zone", "2020-01-02T03:04:05.678Z I NETWORK  [listener] connection accepted\n", formatLegacy},
		{"binary", "\x00\x01\x02\x03ftdc", formatUnknown},
		{"text notes", "case notes: primary was slow\n", formatUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectLogFormat([]byte(tc.head)); got != tc.want {
				t.Fatalf("detectLogFormat=%v want %v", got, tc.want)
			}
		})
	}
}

func TestDiscoverInputsSkipsNonLogFilesInDirectories(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "mongod.log")
	writeFile(t, logPath, []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)+"\n"))
	ftdcPath := filepath.Join(dir, "diagnostic.data", "metrics.2026-07-17T10-00-00Z-00000")
	writeFile(t, ftdcPath, []byte("\x10\x00\x00\x00\x09_id\x00binary-ftdc"))
	legacyPath := filepath.Join(dir, "old", "mongod.log")
	writeFile(t, legacyPath, []byte(legacyLog))

	files, failures, skipped := discoverInputs([]string{dir})
	if len(failures) != 0 {
		t.Fatalf("failures=%v", failures)
	}
	if strings.Join(files, ",") != logPath+","+legacyPath {
		t.Fatalf("files=%v want log and legacy log (legacy fails later with a clear reason)", files)
	}
	if len(skipped) != 1 || skipped[0] != ftdcPath {
		t.Fatalf("skipped=%v want [%s]", skipped, ftdcPath)
	}

	// A file named directly is never skipped silently.
	files, _, skipped = discoverInputs([]string{ftdcPath})
	if len(files) != 1 || len(skipped) != 0 {
		t.Fatalf("explicit file: files=%v skipped=%v", files, skipped)
	}
}

func TestDirectoryWithFTDCIsNotPartialInput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mongod.log"), []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)+"\n"))
	writeFile(t, filepath.Join(dir, "diagnostic.data", "metrics.interim"), []byte("\x00\x01\x02binary"))

	out, code, err := Execute(Config{Report: "timeline", Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{dir}})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	for _, want := range []string{"Input files:          1\n", "Skipped files:        1 not MongoDB logs", "Malformed lines:      0\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Input status: partial") {
		t.Fatalf("skipped FTDC must not make the input partial:\n%s", out)
	}

	out, _, err = Execute(Config{Report: "timeline", JSON: true, Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"filesSkipped": 1`) {
		t.Fatalf("JSON missing filesSkipped:\n%s", out)
	}
}

func TestOnlyNonLogFilesIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "diagnostic.data", "metrics.interim"), []byte("\x00\x01\x02binary"))
	_, code, err := Execute(Config{Report: "queries", Top: -1, Bucket: time.Hour, GroupBy: "optype", SortBy: "total-duration", Inputs: []string{dir}})
	if err == nil || code != 1 {
		t.Fatalf("code=%d err=%v want an error", code, err)
	}
	if !strings.Contains(err.Error(), "no MongoDB log files found (1 non-log files skipped") {
		t.Fatalf("err=%v", err)
	}
}

func TestLegacyTextLogFailsClearly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongod.log")
	writeFile(t, path, []byte(legacyLog))
	for _, report := range []string{"timeline", "queries", "clients", "analysis"} {
		_, code, err := Execute(Config{Report: report, Top: -1, Bucket: time.Hour, GroupBy: "optype", SortBy: "total-duration", MaxBuckets: 100, Inputs: []string{path}})
		if err == nil || code != 1 {
			t.Fatalf("%s: code=%d err=%v want exit 1", report, code, err)
		}
		if !strings.Contains(err.Error(), "legacy text log format (MongoDB 4.2 or older)") {
			t.Fatalf("%s: err=%v", report, err)
		}
	}
}

func TestLegacyFileNextToJSONLogIsPartialInput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "new.log"), []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)+"\n"))
	writeFile(t, filepath.Join(dir, "old.log"), []byte(legacyLog))
	out, _, err := Execute(Config{Report: "timeline", Verbose: true, Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Input status: partial (1/2 files read successfully)") || !strings.Contains(out, errLegacyTextLog.Error()) {
		t.Fatalf("want partial input with legacy reason:\n%s", out)
	}
}

func TestGzipLogsAreRead(t *testing.T) {
	dir := t.TempDir()
	content := slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil) + "\n" +
		slowLine("2026-07-17T10:01:00Z", "insert", "shop", "orders", 200, nil) + "\n"
	writeFile(t, filepath.Join(dir, "mongod.log.2026-07-17T10-00-00.gz"), gzipBytes(t, []byte(content)))
	writeFile(t, filepath.Join(dir, "legacy.log.gz"), gzipBytes(t, []byte(legacyLog)))

	files, _, skipped := discoverInputs([]string{dir})
	if len(files) != 2 || len(skipped) != 0 {
		t.Fatalf("files=%v skipped=%v", files, skipped)
	}
	report := computeTimeline(files, nil, Config{Report: "timeline", Top: -1, Bucket: time.Hour, GroupBy: "optype"})
	if report.Accepted != 2 || report.Stats.Diagnostics.LinesMalformed != 0 {
		t.Fatalf("accepted=%d malformed=%d", report.Accepted, report.Stats.Diagnostics.LinesMalformed)
	}
	var legacyFailed bool
	for _, f := range report.Stats.Failures {
		if strings.HasSuffix(f.Path, "legacy.log.gz") && f.Reason == errLegacyTextLog.Error() {
			legacyFailed = true
		}
	}
	if !legacyFailed {
		t.Fatalf("gzip legacy log not reported: %v", report.Stats.Failures)
	}
}

func TestCorruptGzipIsAFileFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongod.log.gz")
	writeFile(t, path, []byte{0x1f, 0x8b, 0x00, 0x01})
	_, code, err := Execute(Config{Report: "timeline", Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{path}})
	if err == nil || code != 1 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestSlowQueryMatchedByLogID(t *testing.T) {
	withID := func(line string, id int64, msg string) string {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatal(err)
		}
		doc["id"] = id
		doc["msg"] = msg
		data, _ := json.Marshal(doc)
		return string(data)
	}
	base := slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)

	if _, status := parseLineForTest(t, withID(base, 51803, "Slow query")); status != lineAccepted {
		t.Fatalf("id 51803 status=%v want accepted", status)
	}
	// "Slow connection establishment" (id 6496400) mentions slow but is not an operation.
	stats := &PassStats{}
	if _, status := parseSlowOperationLine([]byte(withID(base, 6496400, "Slow connection establishment")), "t.log", 1, stats); status != lineRejected {
		t.Fatalf("id 6496400 status=%v want rejected", status)
	}
	if stats.Diagnostics.SlowQueryLines != 0 || stats.Diagnostics.RejectedSlowOperations != 0 || stats.Diagnostics.NonSlowStructuredLines != 1 {
		t.Fatalf("id 6496400 must count as non-slow: %+v", stats.Diagnostics)
	}
	// Lines without an id fall back to the exact message.
	if _, status := parseLineForTest(t, base); status != lineAccepted {
		t.Fatalf("no-id Slow query status=%v want accepted", status)
	}
	noID := strings.Replace(base, `"msg":"Slow query"`, `"msg":"Slow connection establishment"`, 1)
	if _, status := parseLineForTest(t, noID); status != lineRejected {
		t.Fatalf("no-id other slow message status=%v want rejected", status)
	}
}

func TestClientsReportAuthenticationAcrossVersions(t *testing.T) {
	withID := func(line string, id int64) string {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatal(err)
		}
		doc["id"] = id
		data, _ := json.Marshal(doc)
		return string(data)
	}
	path := filepath.Join(t.TempDir(), "mongod.log")
	lines := []string{
		// 4.4: id 20250, principalName/authenticationDatabase.
		withID(clientEventLine("2026-07-17T10:00:00Z", "conn1", "Connection accepted", map[string]any{"connectionId": 1, "remote": "10.0.0.1:1"}), 22943),
		withID(clientEventLine("2026-07-17T10:00:01Z", "conn1", "Successfully authenticated", map[string]any{"principalName": "legacyUser", "authenticationDatabase": "admin", "mechanism": "SCRAM-SHA-1", "remote": "10.0.0.1:1"}), 20250),
		// 8.0: id 5286306, user/db/client.
		withID(clientEventLine("2026-07-17T10:00:02Z", "conn2", "Connection accepted", map[string]any{"connectionId": 2, "remote": "10.0.0.2:2"}), 22943),
		withID(clientEventLine("2026-07-17T10:00:03Z", "conn2", "Successfully authenticated", map[string]any{"user": "zelmar", "db": "admin", "mechanism": "SCRAM-SHA-256", "client": "10.0.0.2:2"}), 5286306),
		slowLineWithCtx("2026-07-17T10:01:00Z", "conn1", "10.0.0.1:1", "find", "shop", "orders", 100, nil),
		slowLineWithCtx("2026-07-17T10:02:00Z", "conn2", "10.0.0.2:2", "find", "shop", "orders", 100, nil),
	}
	writeFile(t, path, []byte(strings.Join(lines, "\n")+"\n"))

	report := computeClients([]string{path}, nil, Config{Report: "clients", Top: -1, SortBy: "count"})
	got := map[string]bool{}
	for _, row := range report.Rows {
		got[row.User+"@"+row.AuthenticationDatabase] = true
	}
	if !got["legacyUser@admin"] || !got["zelmar@admin"] {
		t.Fatalf("users=%v want legacyUser@admin and zelmar@admin", got)
	}
}

func TestParseTimeArg(t *testing.T) {
	cases := map[string]string{
		"2026-11-04T13:30:00Z":      "2026-11-04T13:30:00Z",
		"2026-11-04T13:30:00-03:00": "2026-11-04T16:30:00Z",
		"2026-11-04T13:30:00.5Z":    "2026-11-04T13:30:00.5Z",
		"2026-11-04T13:30:00":       "2026-11-04T13:30:00Z",
		"2026-11-04T13:30":          "2026-11-04T13:30:00Z",
		"2026-11-04 13:30:00":       "2026-11-04T13:30:00Z",
		"2026-11-04 13:30":          "2026-11-04T13:30:00Z",
		"2026-11-04":                "2026-11-04T00:00:00Z",
	}
	for in, want := range cases {
		got, err := parseTimeArg(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got.UTC().Format(time.RFC3339Nano) != want {
			t.Fatalf("%q: got %s want %s", in, got.UTC().Format(time.RFC3339Nano), want)
		}
	}
	if _, err := parseTimeArg("yesterday"); err == nil {
		t.Fatal("want error for an unparseable time")
	}

	cfg, err := parseCLI([]string{"--report", "timeline", "--from", "2026-11-04T13:30", "--to", "2026-11-04", "x.log"})
	if err == nil {
		t.Fatalf("--from after --to must fail, got %+v", cfg)
	}
	if !strings.Contains(err.Error(), "--from must be <= --to") {
		t.Fatalf("err=%v", err)
	}
}

func TestCmdNamespaceUsesCommandCollection(t *testing.T) {
	cases := []struct {
		ns, op  string
		command map[string]any
		want    string
	}{
		{"config.$cmd", "update", map[string]any{"update": "chunks", "$db": "config"}, "config.chunks"},
		{"shop.$cmd", "delete", map[string]any{"delete": "orders", "$db": "shop"}, "shop.orders"},
		{"admin.$cmd", "internal", map[string]any{"replSetUpdatePosition": 1, "$db": "admin"}, "admin.$cmd"},
		{"shop.orders", "find", map[string]any{"find": "other", "$db": "shop"}, "shop.orders"},
		{"productDb.$cmd", "getMore", map[string]any{"getMore": 1, "collection": "$cmd.aggregate", "$db": "productDb"}, "productDb.$cmd"},
	}
	for _, tc := range cases {
		if got := deriveNamespace(tc.ns, tc.op, tc.command); got != tc.want {
			t.Fatalf("deriveNamespace(%q, %q)=%q want %q", tc.ns, tc.op, got, tc.want)
		}
	}
}

func TestFileLabelsAreShortAndUnique(t *testing.T) {
	got := fileLabels([]string{"/case/rs01-0/mongod.log", "/case/rs01-1/mongod.log", "/case/mongos-0/mongos.log", "/other/a/rs01-0/mongod.log.1"})
	want := map[string]string{
		"/case/rs01-0/mongod.log":      "rs01-0/mongod.log",
		"/case/rs01-1/mongod.log":      "rs01-1/mongod.log",
		"/case/mongos-0/mongos.log":    "mongos.log",
		"/other/a/rs01-0/mongod.log.1": "mongod.log.1",
	}
	for path, label := range want {
		if got[path] != label {
			t.Fatalf("label(%s)=%q want %q", path, got[path], label)
		}
	}
	same := fileLabels([]string{"x/mongod.log", "x/mongod.log"})
	if same["x/mongod.log"] != "x/mongod.log" {
		t.Fatalf("duplicate paths: %v", same)
	}
}

func TestSameNamedLogsFromTwoNodesAreToldApart(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "rs01-0", "mongod.log"), []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)+"\n"))
	writeFile(t, filepath.Join(dir, "rs01-1", "mongod.log"), []byte(slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 900, nil)+"\n"))
	out, _, err := Execute(Config{Report: "queries", Top: -1, Bucket: time.Hour, GroupBy: "optype", SortBy: "total-duration", Inputs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Files:                rs01-0/mongod.log,rs01-1/mongod.log", "rs01-1/mongod.log:1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestContainerLogsWithShellPreambleAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	slow := slowLine("2026-07-17T10:00:00Z", "find", "shop", "orders", 100, nil)
	// PSMDB operator pod log: entrypoint shell trace before mongod's JSON.
	writeFile(t, filepath.Join(dir, "operator", "mongod.log"), []byte("+ exec numactl --interleave=all mongod --bind_ip_all\n"+slow+"\n"))
	// kubectl logs --timestamps / docker logs -t: RFC3339Nano prefix per line.
	writeFile(t, filepath.Join(dir, "kubectl", "mongod.log"), []byte("2026-07-17T10:00:00.123456789Z "+slow+"\n"))

	files, failures, skipped := discoverInputs([]string{dir})
	if len(files) != 2 || len(failures) != 0 || len(skipped) != 0 {
		t.Fatalf("files=%v failures=%v skipped=%v", files, failures, skipped)
	}
	report := computeTimeline(files, nil, Config{Report: "timeline", Top: -1, Bucket: time.Hour, GroupBy: "optype"})
	if report.Accepted != 2 || report.Stats.Diagnostics.LinesMalformed != 1 {
		t.Fatalf("accepted=%d malformed=%d want 2 and 1 (the shell line)", report.Accepted, report.Stats.Diagnostics.LinesMalformed)
	}
}

func TestJSONAndYAMLFilesAreNotLogs(t *testing.T) {
	cases := map[string]string{
		"pretty json":    "{\n  \"ok\": 1\n}\n",
		"compact json":   `{"members":[{"name":"a"}],"ok":1}` + "\n",
		"yaml with json": "apiVersion: v1\nitems:\n  data: |\n    {\n      \"a\": 1\n    }\n",
	}
	for name, content := range cases {
		if got := detectLogFormat([]byte(content)); got != formatUnknown {
			t.Fatalf("%s: format=%v want unknown", name, got)
		}
	}
}
