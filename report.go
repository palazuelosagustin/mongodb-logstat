package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"
)

type reportEnvelope struct {
	SchemaVersion int         `json:"schemaVersion"`
	Report        string      `json:"report"`
	Range         Range       `json:"range"`
	ObservedRange Range       `json:"observedRange"`
	Diagnostics   Diagnostics `json:"diagnostics"`
}

type timelineRowJSON struct {
	Start  time.Time      `json:"start"`
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"`
}

type timelineReportJSON struct {
	reportEnvelope
	GroupBy string            `json:"groupBy"`
	Bucket  string            `json:"bucket"`
	Columns []string          `json:"columns"`
	Rows    []timelineRowJSON `json:"rows"`
}

type summaryTopItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type summaryTotals struct {
	Accepted     int     `json:"accepted"`
	OpsPerMinute float64 `json:"opsPerMinute"`
}

type summaryReportJSON struct {
	reportEnvelope
	Totals           summaryTotals    `json:"totals"`
	LatencyMs        LatencySummary   `json:"latencyMs"`
	TopOperations    []summaryTopItem `json:"topOperations"`
	TopNamespaces    []summaryTopItem `json:"topNamespaces"`
	Plans            map[string]int   `json:"plans"`
	TruncatedRecords int              `json:"truncatedRecords"`
}

type queriesRowJSON struct {
	ShapeID    *string `json:"shapeId"`
	Label      string  `json:"label"`
	Namespace  string  `json:"namespace"`
	Operation  string  `json:"operation"`
	Count      int     `json:"count"`
	AvgMs      float64 `json:"avgMs"`
	P95Ms      int64   `json:"p95Ms"`
	MaxMs      int64   `json:"maxMs"`
	Plan       string  `json:"plan"`
	SampleLine string  `json:"sampleLine"`
}

type queriesReportJSON struct {
	reportEnvelope
	Rows []queriesRowJSON `json:"rows"`
}

type TimelineReport struct {
	Columns  []string
	Rows     []timelineRowJSON
	Observed Range
	Stats    PassStats
}

type SummaryReport struct {
	Accepted     int
	Observed     Range
	Stats        PassStats
	Latency      LatencySummary
	TopOps       []summaryTopItem
	TopNS        []summaryTopItem
	Plans        map[string]int
	Truncated    int
	OpsPerMinute float64
}

type QueriesReport struct {
	Observed Range
	Stats    PassStats
	Rows     []queriesRowJSON
}

type queryMeta struct {
	label     string
	namespace string
	operation string
	shapeID   string
}

func Execute(cfg Config) (string, int, error) {
	files, discoveryFailures := discoverInputs(cfg.Inputs)
	switch cfg.Report {
	case "summary":
		report := computeSummary(files, discoveryFailures, cfg)
		if report.Stats.Diagnostics.FilesRead == 0 {
			return "", 1, fmt.Errorf("no input files could be read")
		}
		return renderSummary(cfg, report)
	case "timeline":
		report := computeTimeline(files, discoveryFailures, cfg)
		if report.Stats.Diagnostics.FilesRead == 0 {
			return "", 1, fmt.Errorf("no input files could be read")
		}
		return renderTimeline(cfg, report)
	case "queries":
		report := computeQueries(files, discoveryFailures, cfg)
		if report.Stats.Diagnostics.FilesRead == 0 {
			return "", 1, fmt.Errorf("no input files could be read")
		}
		return renderQueries(cfg, report)
	default:
		return "", 2, fmt.Errorf("unsupported report: %s", cfg.Report)
	}
}

func renderTimeline(cfg Config, report TimelineReport) (string, int, error) {
	if cfg.JSON {
		body := timelineReportJSON{
			reportEnvelope: newEnvelope("timeline", cfg, report.Observed, report.Stats.Diagnostics),
			GroupBy:        cfg.GroupBy,
			Bucket:         cfg.Bucket.String(),
			Columns:        report.Columns,
			Rows:           report.Rows,
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	buf.WriteString(fmt.Sprintf("Timeline report (%s, bucket %s)\n", cfg.GroupBy, cfg.Bucket))
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}
	bl := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprint(bl, "start\ttotal")
	for _, col := range report.Columns {
		fmt.Fprint(bl, "\t"+col)
	}
	fmt.Fprint(bl, "\n")
	for _, row := range report.Rows {
		fmt.Fprint(bl, row.Start.Format(time.RFC3339))
		fmt.Fprintf(bl, "\t%d", row.Total)
		for _, col := range report.Columns {
			fmt.Fprintf(bl, "\t%d", row.Counts[col])
		}
		fmt.Fprint(bl, "\n")
	}
	if err := bl.Flush(); err != nil {
		return "", 1, err
	}
	return buf.String(), 0, nil
}

func renderSummary(cfg Config, report SummaryReport) (string, int, error) {
	if cfg.JSON {
		body := summaryReportJSON{
			reportEnvelope:   newEnvelope("summary", cfg, report.Observed, report.Stats.Diagnostics),
			Totals:           summaryTotals{Accepted: report.Accepted, OpsPerMinute: report.OpsPerMinute},
			LatencyMs:        report.Latency,
			TopOperations:    report.TopOps,
			TopNamespaces:    report.TopNS,
			Plans:            report.Plans,
			TruncatedRecords: report.Truncated,
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	buf.WriteString("Summary report\n")
	buf.WriteString(fmt.Sprintf("Accepted: %d\n", report.Accepted))
	buf.WriteString(fmt.Sprintf("Ops/min: %.2f\n", report.OpsPerMinute))
	if report.Latency.Min == nil {
		buf.WriteString("Latency ms: n/a\n")
	} else {
		buf.WriteString(fmt.Sprintf(
			"Latency ms: min=%d avg=%.1f p50=%d p95=%d p99=%d max=%d approx=%t\n",
			*report.Latency.Min, *report.Latency.Avg, *report.Latency.P50, *report.Latency.P95, *report.Latency.P99, *report.Latency.Max, report.Latency.Approximate,
		))
	}
	return buf.String(), 0, nil
}

func renderQueries(cfg Config, report QueriesReport) (string, int, error) {
	if cfg.JSON {
		body := queriesReportJSON{
			reportEnvelope: newEnvelope("queries", cfg, report.Observed, report.Stats.Diagnostics),
			Rows:           report.Rows,
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	buf.WriteString("Queries report\n")
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}

	labelWidth := len("label")
	countWidth := len("count")
	avgWidth := len("avg_ms")
	p95Width := len("p95_ms")
	maxWidth := len("max_ms")
	planWidth := len("plan")
	sampleWidth := len("sample_line")

	for _, row := range report.Rows {
		if len(row.Label) > labelWidth {
			labelWidth = len(row.Label)
		}
		if width := len(fmt.Sprintf("%d", row.Count)); width > countWidth {
			countWidth = width
		}
		if width := len(fmt.Sprintf("%.1f", row.AvgMs)); width > avgWidth {
			avgWidth = width
		}
		if width := len(fmt.Sprintf("%d", row.P95Ms)); width > p95Width {
			p95Width = width
		}
		if width := len(fmt.Sprintf("%d", row.MaxMs)); width > maxWidth {
			maxWidth = width
		}
		if len(row.Plan) > planWidth {
			planWidth = len(row.Plan)
		}
		if len(row.SampleLine) > sampleWidth {
			sampleWidth = len(row.SampleLine)
		}
	}

	fmt.Fprintf(&buf, "%-*s  %*s  %*s  %*s  %*s  %-*s  %-*s\n",
		labelWidth, "label",
		countWidth, "count",
		avgWidth, "avg_ms",
		p95Width, "p95_ms",
		maxWidth, "max_ms",
		planWidth, "plan",
		sampleWidth, "sample_line",
	)
	for _, row := range report.Rows {
		fmt.Fprintf(&buf, "%-*s  %*d  %*.1f  %*d  %*d  %-*s  %-*s\n",
			labelWidth, row.Label,
			countWidth, row.Count,
			avgWidth, row.AvgMs,
			p95Width, row.P95Ms,
			maxWidth, row.MaxMs,
			planWidth, row.Plan,
			sampleWidth, row.SampleLine,
		)
	}
	return buf.String(), 0, nil
}

func writeInputStatus(buf *bytes.Buffer, cfg Config, stats PassStats) {
	status := inputStatus(stats.Diagnostics.FilesRead, stats.Diagnostics.FilesFailed)
	if status == "partial" {
		buf.WriteString(fmt.Sprintf("Input status: partial (%d/%d files read successfully)\n", stats.Diagnostics.FilesRead, stats.Diagnostics.FilesDiscovered))
	}
	if cfg.Verbose {
		for _, failure := range stats.Failures {
			buf.WriteString(fmt.Sprintf("stderr: %s: %s\n", failure.Path, failure.Reason))
		}
	}
}

func newEnvelope(report string, cfg Config, observed Range, diagnostics Diagnostics) reportEnvelope {
	return reportEnvelope{
		SchemaVersion: schemaVersion,
		Report:        report,
		Range:         Range{From: cfg.From, To: cfg.To},
		ObservedRange: observed,
		Diagnostics:   diagnostics,
	}
}

func marshalJSON(v any) (string, int, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", 1, err
	}
	return string(data) + "\n", 0, nil
}

func inputStatus(filesRead, filesFailed int) string {
	switch {
	case filesRead == 0:
		return "failed"
	case filesFailed > 0:
		return "partial"
	default:
		return "complete"
	}
}

func computeSummary(files []string, discoveryFailures []FileFailure, cfg Config) SummaryReport {
	stats := PassStats{Failures: append([]FileFailure(nil), discoveryFailures...)}
	stats.Diagnostics.FilesDiscovered = len(files) + len(discoveryFailures)
	stats.Diagnostics.FilesFailed = len(discoveryFailures)

	var hist DurationHistogram
	opCounts := map[string]int{}
	nsCounts := map[string]int{}
	planCounts := map[string]int{}
	truncatedRecords := 0
	accepted := 0
	var observedFrom *time.Time
	var observedTo *time.Time

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		accepted++
		hist.Add(op.Duration)
		opCounts[op.Operation]++
		nsCounts[op.Namespace]++
		planCounts[op.PlanSummary]++
		if op.Truncated {
			truncatedRecords++
		}
		updateObservedRange(&observedFrom, &observedTo, op.Timestamp)
	}, &stats)

	stats.Diagnostics.InputStatus = inputStatus(stats.Diagnostics.FilesRead, stats.Diagnostics.FilesFailed)

	return SummaryReport{
		Accepted:     accepted,
		Observed:     Range{From: observedFrom, To: observedTo},
		Stats:        stats,
		Latency:      hist.Summary(true),
		TopOps:       topCounts(opCounts, cfg.Top),
		TopNS:        topCounts(nsCounts, cfg.Top),
		Plans:        sortedCountMap(planCounts),
		Truncated:    truncatedRecords,
		OpsPerMinute: computeOpsPerMinute(cfg.From, cfg.To, observedFrom, observedTo, accepted),
	}
}

func computeTimeline(files []string, discoveryFailures []FileFailure, cfg Config) TimelineReport {
	pass1 := PassStats{Failures: append([]FileFailure(nil), discoveryFailures...)}
	pass1.Diagnostics.FilesDiscovered = len(files) + len(discoveryFailures)
	pass1.Diagnostics.FilesFailed = len(discoveryFailures)
	groupCounts := map[string]int{}

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		groupCounts[timelineGroupValue(cfg.GroupBy, op)]++
	}, &pass1)

	columns := selectTimelineColumns(groupCounts, cfg.Top)
	columnSet := map[string]struct{}{}
	for _, col := range columns {
		columnSet[col] = struct{}{}
	}

	pass2 := PassStats{Failures: append([]FileFailure(nil), discoveryFailures...)}
	pass2.Diagnostics.FilesDiscovered = len(files) + len(discoveryFailures)
	pass2.Diagnostics.FilesFailed = len(discoveryFailures)
	type bucketRow struct {
		start  time.Time
		total  int
		counts map[string]int
	}
	buckets := map[int64]*bucketRow{}
	var observedFrom *time.Time
	var observedTo *time.Time

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		updateObservedRange(&observedFrom, &observedTo, op.Timestamp)
		bucketStart := op.Timestamp.UTC().Truncate(cfg.Bucket)
		key := bucketStart.UnixNano()
		row := buckets[key]
		if row == nil {
			row = &bucketRow{start: bucketStart, counts: map[string]int{}}
			buckets[key] = row
		}
		row.total++

		group := timelineGroupValue(cfg.GroupBy, op)
		if _, ok := columnSet[group]; !ok && len(columns) > 0 {
			group = "other"
		}
		row.counts[group]++
	}, &pass2)

	rows := make([]timelineRowJSON, 0, len(buckets))
	keys := make([]int64, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, key := range keys {
		row := buckets[key]
		counts := map[string]int{}
		for _, col := range columns {
			counts[col] = row.counts[col]
		}
		if other := row.counts["other"]; other > 0 {
			counts["other"] = other
		}
		rows = append(rows, timelineRowJSON{Start: row.start, Total: row.total, Counts: counts})
	}

	pass2.Diagnostics.InputStatus = inputStatus(pass2.Diagnostics.FilesRead, pass2.Diagnostics.FilesFailed)
	return TimelineReport{
		Columns:  finalizeTimelineColumns(columns, rows),
		Rows:     rows,
		Observed: Range{From: observedFrom, To: observedTo},
		Stats:    pass2,
	}
}

func computeQueries(files []string, discoveryFailures []FileFailure, cfg Config) QueriesReport {
	pass1 := PassStats{Failures: append([]FileFailure(nil), discoveryFailures...)}
	pass1.Diagnostics.FilesDiscovered = len(files) + len(discoveryFailures)
	pass1.Diagnostics.FilesFailed = len(discoveryFailures)
	counts := map[string]int{}
	metadata := map[string]queryMeta{}

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		counts[op.ShapeKey]++
		if _, ok := metadata[op.ShapeKey]; !ok {
			metadata[op.ShapeKey] = queryMeta{label: op.ShapeLabel, namespace: op.Namespace, operation: op.Operation, shapeID: op.ShapeID}
		}
	}, &pass1)

	selected := selectTopShapes(counts, metadata, cfg.Top)
	selectedSet := map[string]queryMeta{}
	for _, shapeKey := range selected {
		selectedSet[shapeKey] = metadata[shapeKey]
	}

	pass2 := PassStats{Failures: append([]FileFailure(nil), discoveryFailures...)}
	pass2.Diagnostics.FilesDiscovered = len(files) + len(discoveryFailures)
	pass2.Diagnostics.FilesFailed = len(discoveryFailures)
	type shapeAgg struct {
		count      int
		hist       DurationHistogram
		planCounts map[string]int
		sampleLine string
	}
	aggregates := map[string]*shapeAgg{}
	var observedFrom *time.Time
	var observedTo *time.Time

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		if _, ok := selectedSet[op.ShapeKey]; !ok {
			return
		}
		updateObservedRange(&observedFrom, &observedTo, op.Timestamp)
		agg := aggregates[op.ShapeKey]
		if agg == nil {
			agg = &shapeAgg{planCounts: map[string]int{}, sampleLine: formatSampleLine(op.SourceFile, op.SourceLine)}
			aggregates[op.ShapeKey] = agg
		}
		agg.count++
		agg.hist.Add(op.Duration)
		agg.planCounts[op.PlanSummary]++
	}, &pass2)

	rows := make([]queriesRowJSON, 0, len(selected))
	for _, shapeKey := range selected {
		agg := aggregates[shapeKey]
		if agg == nil || agg.count == 0 {
			continue
		}
		summary := agg.hist.Summary(false)
		rows = append(rows, queriesRowJSON{
			ShapeID:    nullableString(metadata[shapeKey].shapeID),
			Label:      metadata[shapeKey].label,
			Namespace:  metadata[shapeKey].namespace,
			Operation:  metadata[shapeKey].operation,
			Count:      agg.count,
			AvgMs:      *summary.Avg,
			P95Ms:      *summary.P95,
			MaxMs:      *summary.Max,
			Plan:       dominantPlan(agg.planCounts),
			SampleLine: agg.sampleLine,
		})
	}

	pass2.Diagnostics.InputStatus = inputStatus(pass2.Diagnostics.FilesRead, pass2.Diagnostics.FilesFailed)
	return QueriesReport{
		Observed: Range{From: observedFrom, To: observedTo},
		Stats:    pass2,
		Rows:     rows,
	}
}

func formatSampleLine(path string, line int64) string {
	return fmt.Sprintf("%s:%d", filepath.Base(path), line)
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func topCounts(values map[string]int, limit int) []summaryTopItem {
	type item struct {
		name  string
		count int
	}
	items := make([]item, 0, len(values))
	for name, count := range values {
		items = append(items, item{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].name < items[j].name
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	out := make([]summaryTopItem, 0, len(items))
	for _, item := range items {
		out = append(out, summaryTopItem{Name: item.name, Count: item.count})
	}
	return out
}

func sortedCountMap(values map[string]int) map[string]int {
	if len(values) == 0 {
		return map[string]int{}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]int, len(values))
	for _, key := range keys {
		out[key] = values[key]
	}
	return out
}

func computeOpsPerMinute(from, to, observedFrom, observedTo *time.Time, accepted int) float64 {
	if accepted == 0 {
		return 0
	}

	var minutes float64
	if from != nil && to != nil {
		minutes = to.Sub(*from).Minutes()
		if minutes < 1 {
			minutes = 1
		}
		return float64(accepted) / minutes
	}

	if observedFrom == nil || observedTo == nil {
		return 0
	}

	duration := observedTo.Sub(*observedFrom)
	minutes = float64((duration + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	return float64(accepted) / minutes
}

func updateObservedRange(from, to **time.Time, ts time.Time) {
	value := ts
	if *from == nil || value.Before(**from) {
		*from = &value
	}
	if *to == nil || value.After(**to) {
		*to = &value
	}
}

func timelineGroupValue(groupBy string, op *SlowOperation) string {
	switch groupBy {
	case "namespace":
		return op.Namespace
	case "query":
		return op.ShapeLabel
	default:
		return op.Operation
	}
}

func selectTimelineColumns(counts map[string]int, top int) []string {
	type item struct {
		name  string
		count int
	}
	items := make([]item, 0, len(counts))
	for name, count := range counts {
		items = append(items, item{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].name < items[j].name
	})
	if top > 0 && len(items) > top {
		items = items[:top]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.name)
	}
	return out
}

func finalizeTimelineColumns(columns []string, rows []timelineRowJSON) []string {
	hasOther := false
	for _, row := range rows {
		if row.Counts["other"] > 0 {
			hasOther = true
			break
		}
	}
	out := append([]string(nil), columns...)
	if hasOther && !containsString(out, "other") {
		out = append(out, "other")
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func selectTopShapes(counts map[string]int, metadata map[string]queryMeta, top int) []string {
	type item struct {
		shapeKey string
		count    int
		label    string
	}
	items := make([]item, 0, len(counts))
	for shapeKey, count := range counts {
		items = append(items, item{shapeKey: shapeKey, count: count, label: metadata[shapeKey].label})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].label < items[j].label
	})
	if top > 0 && len(items) > top {
		items = items[:top]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.shapeKey)
	}
	return out
}

func dominantPlan(planCounts map[string]int) string {
	bestPlan := "unknown"
	bestCount := -1
	var names []string
	for name := range planCounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		count := planCounts[name]
		if count > bestCount {
			bestPlan = name
			bestCount = count
		}
	}
	return bestPlan
}
