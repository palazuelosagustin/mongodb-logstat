package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// The events report lists process, replica-set and configuration events in
// time order. It states facts from the log only: no thresholds, levels or
// verdicts. Log ids were checked against MongoDB / Percona Server for
// MongoDB 6.0, 7.0, 8.0 and 8.2 logs.

type EventRow struct {
	Time   time.Time      `json:"-"`
	File   string         `json:"file"`
	Line   int64          `json:"line"`
	Kind   string         `json:"kind"`
	LogID  int64          `json:"logId"`
	Detail string         `json:"detail"`
	Facts  map[string]any `json:"facts,omitempty"`
}

type eventRowJSON struct {
	Time string `json:"time"`
	EventRow
}

type EventsReport struct {
	Observed Range
	Stats    PassStats
	Accepted int
	Rows     []EventRow
	Kinds    map[string]int
}

type eventsReportJSON struct {
	reportEnvelope
	Kinds  map[string]int `json:"kinds"`
	Events []eventRowJSON `json:"events"`
}

type eventSpec struct {
	kind  string
	build func(attr map[string]any) (string, map[string]any)
}

// facts copies the named scalar attributes that are present.
func facts(attr map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if v, ok := attr[key]; ok {
			switch v.(type) {
			case map[string]any, []any:
				continue
			}
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func attrText(attr map[string]any, key string) string {
	v, ok := attr[key]
	if !ok {
		return "?"
	}
	return sanitizeBoundedString(fmt.Sprint(v), 200)
}

var eventSpecs = map[int64]eventSpec{
	4615611: {"start", func(a map[string]any) (string, map[string]any) {
		return fmt.Sprintf("host %s port %s pid %s dbPath %s", attrText(a, "host"), attrText(a, "port"), attrText(a, "pid"), attrText(a, "dbPath")),
			facts(a, "host", "port", "pid", "dbPath")
	}},
	23403: {"version", func(a map[string]any) (string, map[string]any) {
		info, _ := a["buildInfo"].(map[string]any)
		version := attrText(info, "version")
		return version, map[string]any{"version": version}
	}},
	23377: {"signal", func(a map[string]any) (string, map[string]any) {
		return fmt.Sprintf("signal %s (%s)", attrText(a, "signal"), attrText(a, "error")), facts(a, "signal", "error")
	}},
	23138: {"shutdown", func(a map[string]any) (string, map[string]any) {
		return "exitCode " + attrText(a, "exitCode"), facts(a, "exitCode")
	}},
	22271: {"uncleanShutdown", func(a map[string]any) (string, map[string]any) {
		return "detected at startup: lock file not empty", facts(a, "lockFile")
	}},
	21358: {"rsState", func(a map[string]any) (string, map[string]any) {
		return attrText(a, "oldState") + " -> " + attrText(a, "newState"), facts(a, "oldState", "newState")
	}},
	4615652: {"electionStart", func(a map[string]any) (string, map[string]any) {
		return "no PRIMARY seen in election timeout (" + attrText(a, "electionTimeoutPeriodMillis") + " ms)", facts(a, "electionTimeoutPeriodMillis")
	}},
	21450: {"electionWon", func(a map[string]any) (string, map[string]any) {
		return "assuming primary role, term " + attrText(a, "term"), facts(a, "term")
	}},
	21434: {"electionLost", func(a map[string]any) (string, map[string]any) {
		return "dryRun " + attrText(a, "isDryRun"), facts(a, "isDryRun")
	}},
	21215: {"memberState", func(a map[string]any) (string, map[string]any) {
		return attrText(a, "hostAndPort") + " -> " + attrText(a, "newState"), facts(a, "hostAndPort", "newState")
	}},
	21216: {"memberState", func(a map[string]any) (string, map[string]any) {
		f := facts(a, "hostAndPort")
		if f == nil {
			f = map[string]any{}
		}
		f["newState"] = "DOWN"
		return attrText(a, "hostAndPort") + " -> DOWN", f
	}},
	21088: {"syncSource", func(a map[string]any) (string, map[string]any) {
		return attrText(a, "oldSyncSource") + " -> " + attrText(a, "newSyncSource"), facts(a, "oldSyncSource", "newSyncSource")
	}},
	21392: {"rsConfig", func(a map[string]any) (string, map[string]any) {
		config, _ := a["config"].(map[string]any)
		members, _ := config["members"].([]any)
		f := facts(config, "_id", "version", "term")
		if f == nil {
			f = map[string]any{}
		}
		f["members"] = len(members)
		return fmt.Sprintf("set %s version %s term %s, %d members", attrText(config, "_id"), attrText(config, "version"), attrText(config, "term"), len(members)), f
	}},
	21164: {"initialSyncStart", func(a map[string]any) (string, map[string]any) {
		return "attempt " + attrText(a, "initialSyncAttempt") + " of " + attrText(a, "initialSyncMaxAttempts"), facts(a, "initialSyncAttempt", "initialSyncMaxAttempts")
	}},
	21163: {"initialSyncDone", func(a map[string]any) (string, map[string]any) {
		return "took " + attrText(a, "durationSeconds") + " s", facts(a, "durationSeconds")
	}},
	20459: {"fcv", func(a map[string]any) (string, map[string]any) {
		return "setting featureCompatibilityVersion " + attrText(a, "newVersion"), facts(a, "newVersion")
	}},
}

// eventFromLine returns the event for a structured log line, if it is one.
// Every fatal ("s":"F") line is an event.
func eventFromLine(event StructuredEvent) (EventRow, bool) {
	raw := event.Raw
	id, _ := asInt64(raw["id"])
	attr, _ := raw["attr"].(map[string]any)
	if attr == nil {
		attr = map[string]any{}
	}
	row := EventRow{Time: event.Timestamp, File: event.SourceFile, Line: event.SourceLine, LogID: id}
	if spec, ok := eventSpecs[id]; ok {
		row.Kind = spec.kind
		row.Detail, row.Facts = spec.build(attr)
		return row, true
	}
	if asString(raw["s"]) == "F" {
		row.Kind = "fatal"
		row.Detail = sanitizeBoundedString(asString(raw["msg"]), 200)
		row.Facts = facts(attr, "error", "errmsg", "code", "codeName", "file", "line", "msgid")
		return row, true
	}
	return EventRow{}, false
}

func computeEvents(files []string, discoveryFailures []FileFailure, cfg Config) EventsReport {
	report := EventsReport{Stats: newPassStats(files, discoveryFailures), Kinds: map[string]int{}}
	parseFiles(files, parseOptions{
		From: cfg.From,
		To:   cfg.To,
		OnStructuredEvent: func(event StructuredEvent) {
			if row, ok := eventFromLine(event); ok {
				report.Rows = append(report.Rows, row)
				report.Kinds[row.Kind]++
			}
		},
	}, func(op *SlowOperation) {
		report.Accepted++
		updateObservedRange(&report.Observed.From, &report.Observed.To, op.Timestamp)
	}, &report.Stats)
	finalizeStats(&report.Stats)
	sort.SliceStable(report.Rows, func(i, j int) bool {
		a, b := report.Rows[i], report.Rows[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.Before(b.Time)
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return report
}

func renderEvents(cfg Config, report EventsReport) (string, int, error) {
	if cfg.JSON {
		body := eventsReportJSON{
			reportEnvelope: newEnvelope("events", cfg, report.Observed, report.Stats.Diagnostics),
			Kinds:          report.Kinds,
			Events:         make([]eventRowJSON, 0, len(report.Rows)),
		}
		for _, row := range report.Rows {
			body.Events = append(body.Events, eventRowJSON{Time: formatEventTime(row.Time), EventRow: row})
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	writeTerminalMetadata(&buf, cfg, report.Stats, report.Observed, report.Accepted)
	buf.WriteString(fmt.Sprintf("Events report (%d events%s)\n", len(report.Rows), kindSummary(report.Kinds)))
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "time\tfile\tkind\tdetail\n")
	for _, row := range report.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", formatEventTime(row.Time), row.File, row.Kind, row.Detail)
	}
	if err := tw.Flush(); err != nil {
		return "", 1, err
	}
	return buf.String(), 0, nil
}

func formatEventTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// kindSummary renders ": 3 rsState, 1 start" ordered by count, then name.
func kindSummary(kinds map[string]int) string {
	if len(kinds) == 0 {
		return ""
	}
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if kinds[names[i]] != kinds[names[j]] {
			return kinds[names[i]] > kinds[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%d %s", kinds[name], name))
	}
	return ": " + strings.Join(parts, ", ")
}
