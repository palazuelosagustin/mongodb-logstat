package main

import (
	"bytes"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// The messages report groups warning, error and fatal log lines by
// severity, component, log id and message. It counts; it does not judge.

type messageKey struct {
	Severity  string
	Component string
	ID        int64
	Msg       string
}

type messageAgg struct {
	key    messageKey
	count  int
	first  time.Time
	last   time.Time
	files  map[string]int
	sample MessageSample
}

type MessageSample struct {
	File string `json:"file"`
	Line int64  `json:"line"`
	Attr any    `json:"attr,omitempty"`
}

type MessageRow struct {
	Severity  string         `json:"severity"`
	Component string         `json:"component"`
	LogID     int64          `json:"logId"`
	Msg       string         `json:"msg"`
	Count     int            `json:"count"`
	First     string         `json:"first"`
	Last      string         `json:"last"`
	Nodes     map[string]int `json:"nodes"`
	Files     map[string]int `json:"files"`
	Sample    MessageSample  `json:"sample"`
}

type MessagesReport struct {
	Observed      Range
	Stats         PassStats
	Accepted      int
	Rows          []MessageRow
	Lines         int
	BySeverity    map[string]int
	OmittedGroups int
	OmittedLines  int
}

type messagesReportJSON struct {
	reportEnvelope
	Severities    []string       `json:"severities"`
	Lines         int            `json:"lines"`
	BySeverity    map[string]int `json:"bySeverity"`
	Groups        int            `json:"groups"`
	OmittedGroups int            `json:"omittedGroups"`
	OmittedLines  int            `json:"omittedLines"`
	Messages      []MessageRow   `json:"messages"`
}

var messageSeverities = []string{"F", "E", "W"}

func severityRank(s string) int {
	for i, sev := range messageSeverities {
		if s == sev {
			return i
		}
	}
	return len(messageSeverities)
}

// sampleAttrKeysRedacted hold query text or document values; their values
// are replaced by type markers (see redactValue) in message samples.
var sampleAttrKeysRedacted = map[string]bool{
	"command": true, "cmd": true, "originatingCommand": true, "filter": true, "query": true,
	"q": true, "u": true, "pipeline": true, "documents": true, "updates": true, "deletes": true,
	"keyValue": true, "doc": true, "document": true, "o": true, "o2": true, "update": true,
}

const maxSampleString = 300

// sampleAttr bounds attribute values for a message sample: query-like keys
// are redacted to their shape and long strings are cut.
func sampleAttr(v any, depth int) any {
	if depth > 8 {
		return "?truncated"
	}
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			if sampleAttrKeysRedacted[key] {
				out[key], _ = redactValue(child, 0)
				continue
			}
			out[key] = sampleAttr(child, depth+1)
		}
		return out
	case []any:
		limit := len(value)
		if limit > 20 {
			limit = 20
		}
		out := make([]any, 0, limit)
		for _, child := range value[:limit] {
			out = append(out, sampleAttr(child, depth+1))
		}
		return out
	case string:
		if len(value) > maxSampleString {
			cut := maxSampleString
			for cut > 0 && !utf8.RuneStart(value[cut]) {
				cut--
			}
			return value[:cut] + "...(truncated)"
		}
		return value
	default:
		return v
	}
}

func computeMessages(files []string, discoveryFailures []FileFailure, cfg Config) MessagesReport {
	report := MessagesReport{Stats: newPassStats(files, discoveryFailures), BySeverity: map[string]int{}}
	aggs := map[messageKey]*messageAgg{}
	parseFiles(files, parseOptions{
		From: cfg.From,
		To:   cfg.To,
		OnStructuredEvent: func(event StructuredEvent) {
			raw := event.Raw
			sev := asString(raw["s"])
			if severityRank(sev) == len(messageSeverities) {
				return
			}
			id, _ := asInt64(raw["id"])
			key := messageKey{Severity: sev, Component: sanitizeBoundedString(asString(raw["c"]), 40), ID: id, Msg: sanitizeBoundedString(asString(raw["msg"]), 300)}
			agg := aggs[key]
			if agg == nil {
				agg = &messageAgg{key: key, first: event.Timestamp, last: event.Timestamp, files: map[string]int{}}
				agg.sample = MessageSample{File: event.SourceFile, Line: event.SourceLine}
				if attr, ok := raw["attr"]; ok {
					agg.sample.Attr = sampleAttr(attr, 0)
				}
				aggs[key] = agg
			}
			agg.count++
			agg.files[event.SourceFile]++
			if event.Timestamp.Before(agg.first) {
				agg.first = event.Timestamp
			}
			if event.Timestamp.After(agg.last) {
				agg.last = event.Timestamp
			}
			report.Lines++
			report.BySeverity[sev]++
		},
	}, func(op *SlowOperation) {
		report.Accepted++
		updateObservedRange(&report.Observed.From, &report.Observed.To, op.Timestamp)
	}, &report.Stats)
	finalizeStats(&report.Stats)

	list := make([]*messageAgg, 0, len(aggs))
	for _, agg := range aggs {
		list = append(list, agg)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := severityRank(a.key.Severity), severityRank(b.key.Severity); ra != rb {
			return ra < rb
		}
		if a.count != b.count {
			return a.count > b.count
		}
		if a.key.ID != b.key.ID {
			return a.key.ID < b.key.ID
		}
		if a.key.Component != b.key.Component {
			return a.key.Component < b.key.Component
		}
		return a.key.Msg < b.key.Msg
	})
	limit := len(list)
	if cfg.Top > 0 && cfg.Top < limit {
		limit = cfg.Top
	}
	for i, agg := range list {
		if i >= limit {
			report.OmittedGroups++
			report.OmittedLines += agg.count
			continue
		}
		report.Rows = append(report.Rows, MessageRow{
			Severity:  agg.key.Severity,
			Component: agg.key.Component,
			LogID:     agg.key.ID,
			Msg:       agg.key.Msg,
			Count:     agg.count,
			First:     formatEventTime(agg.first),
			Last:      formatEventTime(agg.last),
			Nodes:     nodeCounts(report.Stats, agg.files),
			Files:     agg.files,
			Sample:    agg.sample,
		})
	}
	return report
}

func renderMessages(cfg Config, report MessagesReport) (string, int, error) {
	if cfg.JSON {
		body := messagesReportJSON{
			reportEnvelope: newEnvelope("messages", cfg, report.Observed, report.Stats),
			Severities:     messageSeverities,
			Lines:          report.Lines,
			BySeverity:     report.BySeverity,
			Groups:         len(report.Rows) + report.OmittedGroups,
			OmittedGroups:  report.OmittedGroups,
			OmittedLines:   report.OmittedLines,
			Messages:       report.Rows,
		}
		if body.Messages == nil {
			body.Messages = []MessageRow{}
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	writeTerminalMetadata(&buf, cfg, report.Stats, report.Observed, report.Accepted)
	buf.WriteString(fmt.Sprintf("Messages report (fatal/error/warning: %d F, %d E, %d W lines in %d groups, top %s, omitted_groups %d, omitted_lines %d)\n",
		report.BySeverity["F"], report.BySeverity["E"], report.BySeverity["W"], len(report.Rows)+report.OmittedGroups, topLabel(max(cfg.Top, 0)), report.OmittedGroups, report.OmittedLines))
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "sev\tcomponent\tid\tcount\tfirst\tlast\tnodes\tmsg\n")
	for _, row := range report.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n", row.Severity, row.Component, row.LogID, row.Count, row.First, row.Last, countLabel(row.Nodes, "nodes"), truncateRunes(row.Msg, 90))
	}
	if err := tw.Flush(); err != nil {
		return "", 1, err
	}
	return buf.String(), 0, nil
}

// nodeCounts folds per-file counts into per-node counts.
func nodeCounts(stats PassStats, files map[string]int) map[string]int {
	out := make(map[string]int, len(files))
	for file, n := range files {
		out[stats.nodeOf(file)] += n
	}
	return out
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-3]) + "..."
}
