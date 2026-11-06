package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func processDetails(ts, host string, port int) string {
	return logLine(ts, "I", "CONTROL", 20721, "Process Details", map[string]any{"pid": "1", "port": port, "architecture": "64-bit", "host": host})
}

func optionsLine(ts string, options map[string]any) string {
	return logLine(ts, "I", "CONTROL", 21951, "Options set by command line", map[string]any{"options": options})
}

func buildInfo(ts, version string) string {
	return logLine(ts, "I", "CONTROL", 23403, "Build Info", map[string]any{"buildInfo": map[string]any{"version": version}})
}

func TestNodesFromRotatedFilesAndMongos(t *testing.T) {
	dir := t.TempDir()
	shardOptions := map[string]any{"net": map[string]any{"port": 23508}, "replication": map[string]any{"replSetName": "rs1"}, "sharding": map[string]any{"clusterRole": "shardsvr"}}
	// Two rotated files of one shard member: identity comes from the
	// rotation lines (Process Details, Options, Build Info).
	writeLog(t, filepath.Join(dir, "shard", "mongod.log-20260901"),
		processDetails("2026-09-01T00:00:00.000Z", "db1.example", 23508), buildInfo("2026-09-01T00:00:00.001Z", "6.0.27-21"), optionsLine("2026-09-01T00:00:00.002Z", shardOptions),
		slowLine("2026-09-01T10:00:00Z", "find", "shop", "orders", 100, nil))
	writeLog(t, filepath.Join(dir, "shard", "mongod.log-20260902"),
		processDetails("2026-09-02T00:00:00.000Z", "db1.example", 23508), buildInfo("2026-09-02T00:00:00.001Z", "6.0.27-21"), optionsLine("2026-09-02T00:00:00.002Z", shardOptions),
		slowLine("2026-09-02T10:00:00Z", "find", "shop", "orders", 100, nil),
		slowLine("2026-09-02T11:00:00Z", "insert", "shop", "orders", 100, nil))
	// A mongos started inside the log: no host line, only its options.
	writeLog(t, filepath.Join(dir, "router", "mongos.log"),
		buildInfo("2026-09-01T00:00:00.000Z", "6.0.27-21"),
		optionsLine("2026-09-01T00:00:00.001Z", map[string]any{"net": map[string]any{"port": 27017}, "sharding": map[string]any{"configDB": "cfg/c1:27019"}}),
		slowLine("2026-09-01T10:30:00Z", "find", "shop", "orders", 100, nil))
	// A file with no identity line is its own node, named by its label.
	writeLog(t, filepath.Join(dir, "other", "app.log"), slowLine("2026-09-01T10:45:00Z", "find", "shop", "orders", 100, nil))

	cfg := Config{Report: "timeline", Top: -1, Bucket: 24 * time.Hour, GroupBy: "node", Inputs: []string{dir}}
	out, _, err := Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Nodes: 3",
		"db1.example:23508 mongod rs1 shardsvr 6.0.27-21 2 2026-09-01T00:00:00Z to 2026-09-02T11:00:00Z 3",
		"mongos.log mongos - - 6.0.27-21 1",
		"app.log - - - - 1",
		"Timeline report (node,",
	} {
		if !strings.Contains(squash(out), want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(squash(out), "2026-09-01T00:00:00Z 3 1 1 1") || !strings.Contains(squash(out), "2026-09-02T00:00:00Z 2 2") {
		t.Fatalf("per-node timeline counts wrong:\n%s", out)
	}

	cfg.JSON = true
	out, _, err = Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Nodes []struct {
			Node           string   `json:"node"`
			Host           string   `json:"host"`
			Port           string   `json:"port"`
			Process        string   `json:"process"`
			ReplSet        string   `json:"replSet"`
			ClusterRole    string   `json:"clusterRole"`
			Version        string   `json:"version"`
			Files          []string `json:"files"`
			From           string   `json:"from"`
			To             string   `json:"to"`
			SlowOperations int      `json:"slowOperations"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Nodes) != 3 {
		t.Fatalf("nodes=%+v", body.Nodes)
	}
	shard := body.Nodes[1]
	if shard.Node != "db1.example:23508" || shard.Host != "db1.example" || shard.Port != "23508" || shard.ReplSet != "rs1" ||
		shard.ClusterRole != "shardsvr" || len(shard.Files) != 2 || shard.SlowOperations != 3 || shard.From != "2026-09-01T00:00:00Z" {
		t.Fatalf("shard node: %+v", shard)
	}
}

func TestCopyFileBelongsToOriginalNode(t *testing.T) {
	dir := t.TempDir()
	content := strings.Join([]string{
		processDetails("2026-09-01T00:00:00.000Z", "db1.example", 27017),
		slowLine("2026-09-01T10:00:00Z", "find", "shop", "orders", 100, nil),
	}, "\n") + "\n"
	writeFile(t, filepath.Join(dir, "mongod.log-20260901"), []byte(content))
	writeFile(t, filepath.Join(dir, "mongod.log-20260901.gz"), gzipBytes(t, []byte(content)))
	report := computeEvents([]string{filepath.Join(dir, "mongod.log-20260901"), filepath.Join(dir, "mongod.log-20260901.gz")}, nil, Config{Report: "events", Top: -1})
	if len(report.Stats.Nodes) != 1 || len(report.Stats.Nodes[0].Files) != 2 || report.Stats.Nodes[0].Node != "db1.example:27017" {
		t.Fatalf("nodes=%+v", report.Stats.Nodes)
	}
}

func TestNodesTableHiddenWithoutIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mongod.log")
	writeLog(t, path, slowLine("2026-09-01T10:00:00Z", "find", "shop", "orders", 100, nil))
	out, _, err := Execute(Config{Report: "timeline", Top: -1, Bucket: time.Hour, GroupBy: "optype", Inputs: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Nodes:") {
		t.Fatalf("nodes table without identity:\n%s", out)
	}
}
