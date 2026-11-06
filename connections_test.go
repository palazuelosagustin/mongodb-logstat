package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConnectionsReport(t *testing.T) {
	dir := t.TempDir()
	accepted := func(ts, remote string, count int) string {
		return logLine(ts, "I", "NETWORK", 22943, "Connection accepted", map[string]any{"remote": remote, "connectionId": 1, "connectionCount": count})
	}
	ended := func(ts, remote string, count int) string {
		return logLine(ts, "I", "NETWORK", 22944, "Connection ended", map[string]any{"remote": remote, "connectionCount": count})
	}
	writeLog(t, filepath.Join(dir, "a", "mongod.log"),
		processDetails("2026-09-01T00:00:00.000Z", "db1", 27017),
		accepted("2026-09-01T10:00:00Z", "10.0.0.5:5000", 10),
		logLine("2026-09-01T10:00:01Z", "I", "NETWORK", 51800, "client metadata", map[string]any{"remote": "10.0.0.5:5000", "doc": map[string]any{"application": map[string]any{"name": "orders-api"}, "driver": map[string]any{"name": "nodejs", "version": "6.6"}}}),
		logLine("2026-09-01T10:00:02Z", "I", "ACCESS", 5286306, "Successfully authenticated", map[string]any{"user": "app", "db": "admin", "client": "10.0.0.5:5000"}),
		accepted("2026-09-01T10:05:00Z", "10.0.0.5:5001", 12),
		logLine("2026-09-01T10:05:01Z", "I", "ACCESS", 5286307, "Failed to authenticate", map[string]any{"user": "app", "db": "shop", "mechanism": "SCRAM-SHA-256", "client": "10.0.0.5:5001", "error": "MechanismUnavailable: Unable to use SCRAM-SHA-256 for user app@shop"}),
		ended("2026-09-01T10:30:00Z", "10.0.0.5:5001", 11),
		logLine("2026-09-01T11:10:00Z", "W", "NETWORK", 22942, "Connection refused because there are too many open connections", map[string]any{"remote": "10.0.0.9:6000", "connectionCount": 100}),
		// 6.0 failure form
		logLine("2026-09-01T11:20:00Z", "I", "ACCESS", 20249, "Authentication failed", map[string]any{"principalName": "app", "authenticationDatabase": "shop", "mechanism": "SCRAM-SHA-256", "remote": "10.0.0.6:7000", "error": map[string]any{"code": 18, "codeName": "AuthenticationFailed", "errmsg": "x"}}),
	)
	writeLog(t, filepath.Join(dir, "b", "mongod.log"),
		processDetails("2026-09-01T00:00:00.000Z", "db2", 27017),
		accepted("2026-09-01T10:15:00Z", "10.0.0.7:1", 3),
	)

	cfg := Config{Report: "connections", Top: -1, Bucket: time.Hour, Inputs: []string{dir}}
	out, _, err := Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Connections report (bucket 1h0m0s, gaps skipped): 3 accepted, 1 ended, open max 12, 1 auth succeeded, 2 auth failed, 1 refused",
		"2026-09-01T10:00:00Z db1:27017 2 1 12 11 1 1 0",
		"2026-09-01T10:00:00Z db2:27017 1 0 3 3 0 0 0",
		"2026-09-01T11:00:00Z db1:27017 0 0 - - 0 1 1",
		"10.0.0.5 2 1 1 0 db1:27017 orders-api nodejs 6.6",
		"10.0.0.9 0 0 0 1 db1:27017",
		"app@shop SCRAM-SHA-256 MechanismUnavailable 1",
		"app@shop SCRAM-SHA-256 AuthenticationFailed 1",
	} {
		if !strings.Contains(squash(out), want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	// The free text of the error (which can name users or hosts) is dropped.
	if strings.Contains(out, "Unable to use") {
		t.Fatalf("auth error text leaked:\n%s", out)
	}

	cfg.JSON = true
	cfg.Top = 1
	out, _, err = Execute(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Report string `json:"report"`
		Bucket string `json:"bucket"`
		Totals struct {
			Accepted   int    `json:"accepted"`
			OpenMax    *int64 `json:"openMax"`
			AuthFailed int    `json:"authFailed"`
		} `json:"totals"`
		Rows []struct {
			Start    string `json:"start"`
			Node     string `json:"node"`
			Accepted int    `json:"accepted"`
			OpenLast *int64 `json:"openLast"`
		} `json:"rows"`
		Clients             []ConnectionClient `json:"clients"`
		OmittedClients      int                `json:"omittedClients"`
		AuthFailures        []AuthFailure      `json:"authFailures"`
		OmittedAuthFailures int                `json:"omittedAuthFailures"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	if body.Report != "connections" || body.Bucket != "1h0m0s" || body.Totals.Accepted != 3 || *body.Totals.OpenMax != 12 || body.Totals.AuthFailed != 2 {
		t.Fatalf("body: %+v", body)
	}
	if len(body.Rows) != 3 || body.Rows[0].Start != "2026-09-01T10:00:00Z" || body.Rows[0].Node != "db1:27017" || *body.Rows[0].OpenLast != 11 {
		t.Fatalf("rows: %+v", body.Rows)
	}
	if len(body.Clients) != 1 || body.OmittedClients != 3 || body.Clients[0].ClientIP != "10.0.0.5" || body.Clients[0].Applications[0] != "orders-api" {
		t.Fatalf("clients: %+v omitted %d", body.Clients, body.OmittedClients)
	}
	if len(body.AuthFailures) != 1 || body.OmittedAuthFailures != 1 {
		t.Fatalf("auth failures: %+v omitted %d", body.AuthFailures, body.OmittedAuthFailures)
	}
}
