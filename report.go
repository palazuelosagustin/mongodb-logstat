package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
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

func (r timelineRowJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Start  string         `json:"start"`
		Total  int            `json:"total"`
		Counts map[string]int `json:"counts"`
	}{
		Start:  formatJSONTime(r.Start),
		Total:  r.Total,
		Counts: r.Counts,
	})
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
	Accepted int
}

type QueriesReport struct {
	Observed                 Range
	Stats                    PassStats
	Rows                     []queriesRowJSON
	Accepted                 int
	DisplayedQueryOperations int
	DisplayedDurationMs      int64
	OmittedShapes            int
	OmittedOps               int
	OmittedMs                int64
	Ranking                  string
	TopLimit                 int
	InternalHidden           int
}

type queryMeta struct {
	label          string
	namespace      string
	operation      string
	category       string
	shapeID        string
	identity       ShapeIdentity
	redactedShape  RedactedShape
	internal       bool
	queryHashSrc   string
	truncated      bool
	getMoreAssoc   string
	firstSample    string
	firstTimestamp time.Time
}

type shapeAgg struct {
	count             int
	initialCount      int64
	getMoreCount      int64
	hist              DurationHistogram
	initialDurationMs int64
	getMoreDurationMs int64
	planCounts        map[string]int
	planVariants      map[string]int
	unknownPlanCount  int
	observedOps       map[string]int
	planner           plannerEvidenceState
	metrics           metricBundle
	worst             *SlowOperation
	indicators        map[string]struct{}
	meta              queryMeta
}

type plannerEvidenceState struct {
	hasSortStageEvaluated     int64
	hasSortStageObserved      int64
	fromMultiPlannerEvaluated int64
	fromMultiPlannerObserved  int64
	replannedEvaluated        int64
	replannedObserved         int64
	replanReason              string
}

type metricBundle struct {
	KeysExamined       metricAggState
	DocsExamined       metricAggState
	NReturned          metricAggState
	NMatched           metricAggState
	NModified          metricAggState
	NInserted          metricAggState
	NDeleted           metricAggState
	NumYields          metricAggState
	BytesRead          metricAggState
	BytesWritten       metricAggState
	TimeReadingMicros  metricAggState
	TimeWritingMicros  metricAggState
	PlanningTimeMicros metricAggState
	LockWaitMicros     metricAggState
	CPUNanos           metricAggState
}

func Execute(cfg Config) (string, int, error) {
	files, discoveryFailures, skipped := discoverInputs(cfg.Inputs)
	switch cfg.Report {
	case "timeline":
		report := computeTimeline(files, discoveryFailures, cfg)
		if report.Stats.Diagnostics.FilesRead == 0 {
			return "", 1, noReadableInputError(files, discoveryFailures, skipped)
		}
		report.Stats.Skipped = skipped
		report.Stats.Diagnostics.FilesSkipped = len(skipped)
		return renderTimeline(cfg, report)
	case "queries":
		report := computeQueries(files, discoveryFailures, cfg)
		if report.Stats.Diagnostics.FilesRead == 0 {
			return "", 1, noReadableInputError(files, discoveryFailures, skipped)
		}
		report.Stats.Skipped = skipped
		report.Stats.Diagnostics.FilesSkipped = len(skipped)
		return renderQueries(cfg, report)
	case "clients":
		report := computeClients(files, discoveryFailures, cfg)
		if report.Stats.Diagnostics.FilesRead == 0 {
			return "", 1, noReadableInputError(files, discoveryFailures, skipped)
		}
		report.Stats.Skipped = skipped
		report.Stats.Diagnostics.FilesSkipped = len(skipped)
		return renderClients(cfg, report)
	case "analysis":
		report, err := computeAnalysis(files, discoveryFailures, cfg)
		if err != nil {
			return "", 1, err
		}
		if report.Diagnostics.InputFilesRead == 0 {
			return "", 1, noReadableInputError(files, discoveryFailures, skipped)
		}
		report.Diagnostics.InputFilesSkipped = len(skipped)
		return marshalJSON(report)
	default:
		return "", 2, fmt.Errorf("unsupported report: %s", cfg.Report)
	}
}

// noReadableInputError explains why nothing could be read, so a wrong input
// (legacy text log, FTDC directory, typo) is not mistaken for an empty log.
func noReadableInputError(files []string, discoveryFailures []FileFailure, skipped []string) error {
	reasons := append([]FileFailure(nil), discoveryFailures...)
	for _, path := range files {
		format, err := sniffLogFile(path)
		switch {
		case err != nil:
			reasons = append(reasons, FileFailure{Path: path, Reason: err.Error()})
		case format == formatLegacy:
			reasons = append(reasons, FileFailure{Path: path, Reason: errLegacyTextLog.Error()})
		case format == formatUnknown:
			reasons = append(reasons, FileFailure{Path: path, Reason: errNotMongoLog.Error()})
		}
	}
	var b strings.Builder
	b.WriteString("no input files could be read")
	if len(files) == 0 && len(discoveryFailures) == 0 && len(skipped) > 0 {
		fmt.Fprintf(&b, ": no MongoDB log files found (%d non-log files skipped, e.g. %s)", len(skipped), skipped[0])
		return errors.New(b.String())
	}
	const maxShown = 3
	for i, r := range reasons {
		if i == maxShown {
			fmt.Fprintf(&b, "\n  ... and %d more", len(reasons)-maxShown)
			break
		}
		fmt.Fprintf(&b, "\n  %s: %s", r.Path, r.Reason)
	}
	return errors.New(b.String())
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
	writeTerminalMetadata(&buf, cfg, report.Stats, report.Observed, report.Accepted)
	buf.WriteString(fmt.Sprintf("Timeline report (%s, bucket %s, gaps skipped)\n", cfg.GroupBy, cfg.Bucket))
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
	writeTerminalMetadata(&buf, cfg, report.Stats, report.Observed, report.Accepted)
	buf.WriteString(fmt.Sprintf("Queries report (ranking %s, top %s, displayed_query_ops %d, omitted_query_ops %d, omitted_shapes %d, omitted_ms %d, omitted_duration_share %.2f%%)\n", report.Ranking, topLabel(report.TopLimit), report.DisplayedQueryOperations, report.OmittedOps, report.OmittedShapes, report.OmittedMs, percentOf(report.OmittedMs, report.DisplayedDurationMs+report.OmittedMs)))
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}

	bl := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(bl, "label\tcount\tavg_ms\tp95_ms\tmax_ms\tplan\tsample_line")
	for _, row := range report.Rows {
		fmt.Fprintf(bl, "%s\t%d\t%.1f\t%d\t%d\t%s\t%s\n", row.Label, row.Count, row.AvgMs, row.P95Ms, row.MaxMs, row.Plan, row.SampleLine)
	}
	if err := bl.Flush(); err != nil {
		return "", 1, err
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
		for _, path := range stats.Skipped {
			buf.WriteString(fmt.Sprintf("stderr: %s: skipped, not a MongoDB log\n", path))
		}
	}
}

func writeTerminalMetadata(buf *bytes.Buffer, cfg Config, stats PassStats, slowRange Range, accepted int) {
	buf.WriteString(fmt.Sprintf("Input files:          %d\n", stats.Diagnostics.FilesDiscovered))
	if len(stats.Skipped) > 0 {
		buf.WriteString(fmt.Sprintf("Skipped files:        %d not MongoDB logs (--verbose lists them)\n", len(stats.Skipped)))
	}
	buf.WriteString(fmt.Sprintf("Files:                %s\n", strings.Join(stats.InputFiles, ",")))
	buf.WriteString(fmt.Sprintf("Requested range:      %s to %s\n", formatRequestedTime(cfg.From), formatRequestedTime(cfg.To)))
	buf.WriteString(fmt.Sprintf("Input log range:      %s to %s\n", formatMaybeTime(stats.InputObservedFrom), formatMaybeTime(stats.InputObservedTo)))
	buf.WriteString(fmt.Sprintf("Slow-operation range: %s to %s\n", formatMaybeTime(slowRange.From), formatMaybeTime(slowRange.To)))
	buf.WriteString(fmt.Sprintf("Analysis duration:    %s\n", analysisDurationLabel(cfg, stats, slowRange)))
	buf.WriteString(fmt.Sprintf("Logged slow ops:      %d\n", accepted))
	buf.WriteString(fmt.Sprintf("Non-slow structured:  %d\n", stats.Diagnostics.NonSlowStructuredLines))
	buf.WriteString(fmt.Sprintf("Malformed lines:      %d\n", stats.Diagnostics.LinesMalformed))
	buf.WriteString(fmt.Sprintf("Rejected slow ops:    %d\n", stats.Diagnostics.RejectedSlowOperations))
}

func analysisDurationLabel(cfg Config, stats PassStats, slowRange Range) string {
	if cfg.From != nil && cfg.To != nil {
		return cfg.To.Sub(*cfg.From).String()
	}
	from, to := stats.InputObservedFrom, stats.InputObservedTo
	if from == nil || to == nil {
		from, to = slowRange.From, slowRange.To
	}
	if from == nil || to == nil {
		return "unknown"
	}
	return to.Sub(*from).String()
}

func formatMaybeTime(ts *time.Time) string {
	if ts == nil {
		return "unknown"
	}
	return ts.UTC().Format(time.RFC3339)
}

func formatRequestedTime(ts *time.Time) string {
	if ts == nil {
		return "none"
	}
	return ts.UTC().Format(time.RFC3339)
}

func newEnvelope(report string, cfg Config, observed Range, diagnostics Diagnostics) reportEnvelope {
	diagnostics.InputStatus = inputStatus(diagnostics.FilesRead, diagnostics.FilesFailed)
	diagnostics.PartialInput = diagnostics.InputStatus == "partial"
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

func computeTimeline(files []string, discoveryFailures []FileFailure, cfg Config) TimelineReport {
	pass1 := newPassStats(files, discoveryFailures)
	groupCounts := map[string]int{}

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		groupCounts[timelineGroupValue(cfg.GroupBy, op)]++
	}, &pass1)

	columns := selectTimelineColumns(groupCounts, effectiveTop(cfg, "timeline"))
	columnSet := map[string]struct{}{}
	for _, col := range columns {
		columnSet[col] = struct{}{}
	}

	pass2 := newPassStats(files, discoveryFailures)
	type bucketRow struct {
		start  time.Time
		total  int
		counts map[string]int
	}
	buckets := map[int64]*bucketRow{}
	var observedFrom *time.Time
	var observedTo *time.Time
	accepted := 0

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		accepted++
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

	finalizeStats(&pass2)
	return TimelineReport{
		Columns:  finalizeTimelineColumns(columns, rows),
		Rows:     rows,
		Observed: Range{From: observedFrom, To: observedTo},
		Stats:    pass2,
		Accepted: accepted,
	}
}

func computeQueries(files []string, discoveryFailures []FileFailure, cfg Config) QueriesReport {
	pass1 := newPassStats(files, discoveryFailures)
	aggs := map[string]*shapeAgg{}
	accepted := 0
	internalHidden := 0
	internalHiddenMs := int64(0)

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		accepted++
		if op.Internal && !cfg.IncludeInternal {
			internalHidden++
			internalHiddenMs += op.DurationMillis
			return
		}
		agg := aggs[op.ShapeKey]
		if agg == nil {
			agg = newShapeAgg(op)
			aggs[op.ShapeKey] = agg
		}
		agg.add(op)
	}, &pass1)

	selected := selectShapesByRanking(aggs, effectiveTop(cfg, "queries"), cfg.SortBy)
	selectedSet := map[string]struct{}{}
	for _, shapeKey := range selected {
		selectedSet[shapeKey] = struct{}{}
	}

	pass2 := newPassStats(files, discoveryFailures)
	selectedAggs := map[string]*shapeAgg{}
	var observedFrom *time.Time
	var observedTo *time.Time
	selectedAccepted := 0
	selectedDurationMs := int64(0)

	parseFiles(files, parseOptions{From: cfg.From, To: cfg.To}, func(op *SlowOperation) {
		if op.Internal && !cfg.IncludeInternal {
			return
		}
		if _, ok := selectedSet[op.ShapeKey]; !ok {
			return
		}
		selectedAccepted++
		selectedDurationMs += op.DurationMillis
		updateObservedRange(&observedFrom, &observedTo, op.Timestamp)
		agg := selectedAggs[op.ShapeKey]
		if agg == nil {
			agg = newShapeAgg(op)
			selectedAggs[op.ShapeKey] = agg
		}
		agg.add(op)
	}, &pass2)

	labels := uniqueShapeLabels(selected, selectedAggs)
	rows := make([]queriesRowJSON, 0, len(selected))
	for _, shapeKey := range selected {
		agg := selectedAggs[shapeKey]
		if agg == nil || agg.count == 0 {
			continue
		}
		summary := agg.hist.Summary(false)
		rows = append(rows, queriesRowJSON{
			ShapeID:    nullableString(agg.meta.shapeID),
			Label:      labels[shapeKey],
			Namespace:  agg.meta.namespace,
			Operation:  agg.meta.operation,
			Count:      agg.count,
			AvgMs:      *summary.Avg,
			P95Ms:      *summary.P95,
			MaxMs:      *summary.Max,
			Plan:       dominantPlan(agg.planCounts),
			SampleLine: formatSampleLine(agg.worst.SourceFile, agg.worst.SourceLine),
		})
	}

	finalizeStats(&pass2)
	omittedShapes, omittedOps, omittedMs := omittedShapeTotals(aggs, selectedSet)
	omittedOps += internalHidden
	omittedMs += internalHiddenMs
	return QueriesReport{
		Observed:                 Range{From: observedFrom, To: observedTo},
		Stats:                    pass2,
		Rows:                     rows,
		Accepted:                 accepted,
		DisplayedQueryOperations: selectedAccepted,
		DisplayedDurationMs:      selectedDurationMs,
		OmittedShapes:            omittedShapes,
		OmittedOps:               omittedOps,
		OmittedMs:                omittedMs,
		Ranking:                  cfg.SortBy,
		TopLimit:                 effectiveTop(cfg, "queries"),
		InternalHidden:           internalHidden,
	}
}

func newPassStats(files []string, discoveryFailures []FileFailure) PassStats {
	stats := PassStats{
		Failures:   append([]FileFailure(nil), discoveryFailures...),
		InputFiles: inputFileNames(files, discoveryFailures),
	}
	stats.Diagnostics.RejectedByReason = fixedRejectedByReasonMap()
	stats.Diagnostics.FilesDiscovered = len(files) + len(discoveryFailures)
	stats.Diagnostics.FilesFailed = len(discoveryFailures)
	return stats
}

func inputFileNames(files []string, discoveryFailures []FileFailure) []string {
	names := make([]string, 0, len(files)+len(discoveryFailures))
	for _, file := range files {
		names = append(names, filepath.Base(file))
	}
	for _, failure := range discoveryFailures {
		names = append(names, filepath.Base(failure.Path))
	}
	return names
}

func finalizeStats(stats *PassStats) {
	stats.Diagnostics.InputStatus = inputStatus(stats.Diagnostics.FilesRead, stats.Diagnostics.FilesFailed)
	stats.Diagnostics.PartialInput = stats.Diagnostics.InputStatus == "partial"
}

func newShapeAgg(op *SlowOperation) *shapeAgg {
	return &shapeAgg{
		planCounts:   map[string]int{},
		planVariants: map[string]int{},
		observedOps:  map[string]int{},
		indicators:   map[string]struct{}{},
		meta: queryMeta{
			label:         op.ShapeLabel,
			namespace:     op.Namespace,
			operation:     op.OriginOperation,
			category:      op.OriginCategory,
			shapeID:       op.ShapeID,
			identity:      op.ShapeIdentity,
			redactedShape: op.RedactedShape,
			internal:      op.Internal,
			queryHashSrc:  op.QueryHashSource,
			truncated:     op.Truncated,
			getMoreAssoc:  getMoreAssociation(op),
			firstSample:   formatSampleLine(op.SourceFile, op.SourceLine),
		},
	}
}

func (a *shapeAgg) add(op *SlowOperation) {
	a.count++
	a.hist.Add(op.Duration)
	a.observedOps[op.Operation]++
	if op.Operation == "getMore" {
		a.getMoreCount++
		a.getMoreDurationMs += op.DurationMillis
	} else {
		a.initialCount++
		a.initialDurationMs += op.DurationMillis
	}
	a.planCounts[op.PlanSummary]++
	if isUnknownPlanEvidence(op) {
		a.unknownPlanCount++
	} else {
		variant := op.PlanSummary + "\x00" + op.PlanCacheKey + "\x00" + op.QueryFramework
		a.planVariants[variant]++
	}
	a.addPlannerEvidence(op)
	a.metrics.add(op)
	if a.worst == nil || op.DurationMillis > a.worst.DurationMillis || (op.DurationMillis == a.worst.DurationMillis && formatSampleLine(op.SourceFile, op.SourceLine) < formatSampleLine(a.worst.SourceFile, a.worst.SourceLine)) {
		copy := *op
		a.worst = &copy
	}
	for _, indicator := range evidenceIndicators(op) {
		a.indicators[indicator] = struct{}{}
	}
	if op.Truncated {
		a.indicators["truncated_evidence"] = struct{}{}
		a.meta.truncated = true
	}
	if op.Operation == "getMore" {
		if op.GetMoreAssociated {
			a.meta.getMoreAssoc = "originatingCommand"
		} else {
			a.indicators["unassociated_getmore"] = struct{}{}
			a.meta.getMoreAssoc = "unavailable"
		}
	}
}

func isUnknownPlanEvidence(op *SlowOperation) bool {
	return (op.PlanSummary == "" || op.PlanSummary == "unknown" || op.PlanSummary == "?string") && op.PlanCacheKey == "" && op.QueryFramework == ""
}

func (a *shapeAgg) addPlannerEvidence(op *SlowOperation) {
	if op.HasSortStage.Valid {
		a.planner.hasSortStageEvaluated++
		if op.HasSortStage.Value {
			a.planner.hasSortStageObserved++
		}
	}
	if op.FromMultiPlanner.Valid {
		a.planner.fromMultiPlannerEvaluated++
		if op.FromMultiPlanner.Value {
			a.planner.fromMultiPlannerObserved++
		}
	}
	if op.Replanned.Valid {
		a.planner.replannedEvaluated++
		if op.Replanned.Value {
			a.planner.replannedObserved++
		}
	}
	if op.ReplanReason != "" && a.planner.replanReason == "" {
		a.planner.replanReason = op.ReplanReason
	}
}

func (m *metricBundle) add(op *SlowOperation) {
	m.KeysExamined.Add(op.KeysExamined)
	m.DocsExamined.Add(op.DocsExamined)
	m.NReturned.Add(op.NReturned)
	m.NMatched.Add(op.NMatched)
	m.NModified.Add(op.NModified)
	m.NInserted.Add(op.NInserted)
	m.NDeleted.Add(op.NDeleted)
	m.NumYields.Add(op.NumYields)
	m.BytesRead.Add(op.BytesRead)
	m.BytesWritten.Add(op.BytesWritten)
	m.TimeReadingMicros.Add(op.TimeReadingMicros)
	m.TimeWritingMicros.Add(op.TimeWritingMicros)
	m.PlanningTimeMicros.Add(op.PlanningTimeMicros)
	m.LockWaitMicros.Add(op.LockWaitMicros)
	m.CPUNanos.Add(op.CPUNanos)
}

func evidenceIndicators(op *SlowOperation) []string {
	var out []string
	if op.PlanSummary == "COLLSCAN" {
		out = append(out, "collection_scan_observed")
	}
	if op.UsedDisk.Valid && op.UsedDisk.Value {
		out = append(out, "disk_spill_observed")
	}
	if op.Replanned.Valid && op.Replanned.Value {
		out = append(out, "replanned_observed")
	}
	if op.LockWaitMicros.Valid && op.LockWaitMicros.Value > 0 {
		out = append(out, "lock_wait_observed")
	}
	if op.DocsExamined.Valid && op.NReturned.Valid && op.NReturned.Value > 0 && op.DocsExamined.Value >= 1000 && op.DocsExamined.Value/op.NReturned.Value >= 100 {
		out = append(out, "high_scan_amplification_observed")
	}
	return out
}

func getMoreAssociation(op *SlowOperation) string {
	if op.Operation != "getMore" {
		return "not_getmore"
	}
	if op.GetMoreAssociated {
		return "originatingCommand"
	}
	return "unavailable"
}

func omittedShapeTotals(aggs map[string]*shapeAgg, selected map[string]struct{}) (int, int, int64) {
	var shapes int
	var ops int
	var ms int64
	for key, agg := range aggs {
		if _, ok := selected[key]; ok {
			continue
		}
		shapes++
		ops += agg.count
		ms += agg.hist.TotalMs()
	}
	return shapes, ops, ms
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

func sortedInt64Map(values map[string]int64) map[string]int64 {
	out := fixedRejectedByReasonMap()
	for key, value := range values {
		out[key] = value
	}
	keys := make([]string, 0, len(out))
	for key := range out {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sorted := make(map[string]int64, len(out))
	for _, key := range keys {
		sorted[key] = out[key]
	}
	return sorted
}

func supportedIndicatorKeys() []string {
	return []string{"collection_scan_observed", "disk_spill_observed", "high_scan_amplification_observed", "lock_wait_observed", "multiple_plan_variants_observed", "replanned_observed", "truncated_evidence", "unassociated_getmore"}
}

func shapeIndicatorSet(agg *shapeAgg) map[string]struct{} {
	out := map[string]struct{}{}
	for indicator := range agg.indicators {
		out[indicator] = struct{}{}
	}
	if len(agg.planVariants) > 1 {
		out["multiple_plan_variants_observed"] = struct{}{}
	}
	return out
}

func analysisIndicators(aggs map[string]*shapeAgg) AnalysisIndicators {
	shapeCounts := map[string]int{}
	operationCounts := map[string]int{}
	support := map[string]bool{}
	for _, key := range supportedIndicatorKeys() {
		shapeCounts[key] = 0
		operationCounts[key] = 0
		support[key] = true
	}
	for _, agg := range aggs {
		for indicator := range shapeIndicatorSet(agg) {
			shapeCounts[indicator]++
			operationCounts[indicator] += agg.count
		}
	}
	return AnalysisIndicators{ShapeCounts: sortedCountMap(shapeCounts), OperationCounts: sortedCountMap(operationCounts), IndicatorSupport: sortedBoolMap(support)}
}

func sortedBoolMap(values map[string]bool) map[string]bool {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]bool, len(values))
	for _, key := range keys {
		out[key] = values[key]
	}
	return out
}

func analysisRates(accepted int64, durationSeconds int64) AnalysisRates {
	den := "analysisDurationSeconds"
	if durationSeconds <= 0 {
		den = "unknown"
		return AnalysisRates{OverAnalysisRange: AnalysisRate{Denominator: den}}
	}
	seconds := durationSeconds
	rate := float64(accepted) / (float64(durationSeconds) / 60)
	return AnalysisRates{OverAnalysisRange: AnalysisRate{LoggedSlowOperationsPerMinute: &rate, DenominatorSeconds: &seconds, Denominator: den}}
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
	case "category":
		return op.Category
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

func selectShapesByRanking(aggs map[string]*shapeAgg, top int, sortBy string) []string {
	keys := make([]string, 0, len(aggs))
	for key := range aggs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a := aggs[keys[i]]
		b := aggs[keys[j]]
		av := rankingValue(a, sortBy)
		bv := rankingValue(b, sortBy)
		if av != bv {
			return av > bv
		}
		ad := a.hist.TotalMs()
		bd := b.hist.TotalMs()
		if ad != bd {
			return ad > bd
		}
		if a.meta.namespace != b.meta.namespace {
			return a.meta.namespace < b.meta.namespace
		}
		if a.meta.operation != b.meta.operation {
			return a.meta.operation < b.meta.operation
		}
		return a.meta.shapeID < b.meta.shapeID
	})
	if top > 0 && len(keys) > top {
		keys = keys[:top]
	}
	return keys
}

func rankingValue(agg *shapeAgg, sortBy string) int64 {
	summary := agg.hist.Summary(false)
	switch sortBy {
	case "count":
		return int64(agg.count)
	case "p95":
		if summary.P95 == nil {
			return 0
		}
		return *summary.P95
	case "max":
		if summary.Max == nil {
			return 0
		}
		return *summary.Max
	case "docs-examined":
		return agg.metrics.DocsExamined.sum
	default:
		return agg.hist.TotalMs()
	}
}

func uniqueShapeLabels(keys []string, aggs map[string]*shapeAgg) map[string]string {
	labels := map[string]string{}
	prefixLen := 12
	for {
		seen := map[string]string{}
		collision := false
		for _, key := range keys {
			agg := aggs[key]
			if agg == nil {
				continue
			}
			id := agg.meta.shapeID
			if id == "" {
				id = stableHash(agg.meta.operation, agg.meta.namespace, key)
			}
			prefix := id
			if len(prefix) > prefixLen {
				prefix = prefix[:prefixLen]
			}
			label := fmt.Sprintf("%s:%s#%s", agg.meta.operation, agg.meta.namespace, prefix)
			if previous, ok := seen[label]; ok && previous != key {
				collision = true
				break
			}
			seen[label] = key
			labels[key] = label
		}
		if !collision || prefixLen >= 64 {
			return labels
		}
		prefixLen += 4
	}
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

func effectiveTop(cfg Config, context string) int {
	if cfg.Top >= 0 {
		return cfg.Top
	}
	if context == "queries" || context == "analysis" || (context == "timeline" && cfg.GroupBy == "query") {
		return 20
	}
	return 0
}

func topLabel(top int) string {
	if top == 0 {
		return "all"
	}
	return fmt.Sprintf("%d", top)
}

// Analysis schema v2.
type AnalysisReport struct {
	SchemaVersion       int                  `json:"schemaVersion"`
	Report              string               `json:"report"`
	GeneratedAt         string               `json:"generatedAt"`
	Semantics           AnalysisSemantics    `json:"semantics"`
	Ranges              AnalysisRanges       `json:"ranges"`
	Options             AnalysisOptions      `json:"options"`
	Diagnostics         AnalysisDiagnostics  `json:"diagnostics"`
	Global              AnalysisGlobal       `json:"global"`
	Timeline            []AnalysisBucket     `json:"timeline"`
	HotWindows          []AnalysisHotWindow  `json:"hotWindows"`
	QueryShapes         []AnalysisQueryShape `json:"queryShapes"`
	Namespaces          []AnalysisNamespace  `json:"namespaces"`
	InternalOperations  []AnalysisInternalOp `json:"internalOperations"`
	Omitted             AnalysisOmitted      `json:"omitted"`
	IndicatorThresholds map[string]any       `json:"indicatorThresholds"`
}

type AnalysisSemantics struct {
	Dataset                 string         `json:"dataset"`
	RepresentsTotalWorkload bool           `json:"representsTotalWorkload"`
	CountsMayBeSampled      bool           `json:"countsMayBeSampled"`
	DurationMeaning         string         `json:"durationMeaning"`
	Timezone                string         `json:"timezone"`
	EmptyBucketMeaning      string         `json:"emptyBucketMeaning"`
	UntrustedInputSanitized bool           `json:"untrustedInputSanitized"`
	SlowOpConfiguration     map[string]any `json:"slowOpConfiguration"`
}

type AnalysisRanges struct {
	Requested               AnalysisRequestedRange `json:"requested"`
	InputObserved           AnalysisObservedRange  `json:"inputObserved"`
	SlowOperationsObserved  AnalysisObservedRange  `json:"slowOperationsObserved"`
	AnalysisDurationSeconds int64                  `json:"analysisDurationSeconds"`
}

type AnalysisRequestedRange struct {
	From          *time.Time `json:"from"`
	To            *time.Time `json:"to"`
	FromInclusive bool       `json:"fromInclusive"`
	ToExclusive   bool       `json:"toExclusive"`
}

type AnalysisObservedRange struct {
	First *time.Time `json:"first"`
	Last  *time.Time `json:"last"`
}

func (r AnalysisRequestedRange) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		From          *string `json:"from"`
		To            *string `json:"to"`
		FromInclusive bool    `json:"fromInclusive"`
		ToExclusive   bool    `json:"toExclusive"`
	}{
		From:          formatJSONTimePtr(r.From),
		To:            formatJSONTimePtr(r.To),
		FromInclusive: r.FromInclusive,
		ToExclusive:   r.ToExclusive,
	})
}

func (r AnalysisObservedRange) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		First *string `json:"first"`
		Last  *string `json:"last"`
	}{
		First: formatJSONTimePtr(r.First),
		Last:  formatJSONTimePtr(r.Last),
	})
}

type AnalysisOptions struct {
	Bucket          string `json:"bucket"`
	Top             int    `json:"top"`
	SortBy          string `json:"sortBy"`
	IncludeInternal bool   `json:"includeInternal"`
	FillGaps        bool   `json:"fillGaps"`
	MaxBuckets      int    `json:"maxBuckets"`
}

type AnalysisDiagnostics struct {
	InputFilesDiscovered      int              `json:"inputFilesDiscovered"`
	InputFilesRead            int              `json:"inputFilesRead"`
	InputFilesFailed          int              `json:"inputFilesFailed"`
	InputFilesSkipped         int              `json:"inputFilesSkipped"`
	LinesRead                 int64            `json:"linesRead"`
	ValidStructuredLines      int64            `json:"validStructuredLines"`
	SlowQueryLines            int64            `json:"slowQueryLines"`
	AcceptedSlowOperations    int64            `json:"acceptedSlowOperations"`
	NonSlowStructuredLines    int64            `json:"nonSlowStructuredLines"`
	FilteredByTime            int64            `json:"filteredByTime"`
	FilteredByDuration        int64            `json:"filteredByDuration"`
	MalformedLines            int64            `json:"malformedLines"`
	RejectedSlowOperations    int64            `json:"rejectedSlowOperations"`
	RejectedByReason          map[string]int64 `json:"rejectedByReason"`
	TruncatedSlowOperations   int64            `json:"truncatedSlowOperations"`
	CanonicalizationFallbacks int64            `json:"canonicalizationFallbacks"`
	CanonicalizationFailures  int64            `json:"canonicalizationFailures"`
	UnassociatedGetMore       int64            `json:"unassociatedGetMore"`
	PartialInput              bool             `json:"partialInput"`
	InputStatus               string           `json:"inputStatus"`
}

type AnalysisGlobal struct {
	LoggedSlowOperations int64              `json:"loggedSlowOperations"`
	CumulativeDurationMs int64              `json:"cumulativeDurationMs"`
	LatencyMs            LatencySummary     `json:"latencyMs"`
	CountsByCategory     map[string]int     `json:"countsByCategory"`
	CountsByOperation    map[string]int     `json:"countsByOperation"`
	Indicators           AnalysisIndicators `json:"indicators"`
	InternalCount        int64              `json:"internalCount"`
	Rates                AnalysisRates      `json:"rates"`
}

type AnalysisIndicators struct {
	ShapeCounts      map[string]int  `json:"shapeCounts"`
	OperationCounts  map[string]int  `json:"operationCounts"`
	IndicatorSupport map[string]bool `json:"indicatorSupport"`
}

type AnalysisRates struct {
	OverAnalysisRange AnalysisRate `json:"overAnalysisRange"`
}

type AnalysisRate struct {
	LoggedSlowOperationsPerMinute *float64 `json:"loggedSlowOperationsPerMinute"`
	DenominatorSeconds            *int64   `json:"denominatorSeconds"`
	Denominator                   string   `json:"denominator"`
}

type AnalysisBucket struct {
	Start                time.Time      `json:"start"`
	End                  time.Time      `json:"end"`
	ValidLogLines        int64          `json:"validLogLines"`
	LoggedSlowOperations int            `json:"loggedSlowOperations"`
	CumulativeDurationMs int64          `json:"cumulativeDurationMs"`
	LatencyP95Ms         *int64         `json:"latencyP95Ms"`
	Operations           map[string]int `json:"operations"`
	DominantShapeKeys    []string       `json:"dominantShapeKeys"`
}

func (b AnalysisBucket) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Start                string         `json:"start"`
		End                  string         `json:"end"`
		ValidLogLines        int64          `json:"validLogLines"`
		LoggedSlowOperations int            `json:"loggedSlowOperations"`
		CumulativeDurationMs int64          `json:"cumulativeDurationMs"`
		LatencyP95Ms         *int64         `json:"latencyP95Ms"`
		Operations           map[string]int `json:"operations"`
		DominantShapeKeys    []string       `json:"dominantShapeKeys"`
	}{
		Start:                formatJSONTime(b.Start),
		End:                  formatJSONTime(b.End),
		ValidLogLines:        b.ValidLogLines,
		LoggedSlowOperations: b.LoggedSlowOperations,
		CumulativeDurationMs: b.CumulativeDurationMs,
		LatencyP95Ms:         b.LatencyP95Ms,
		Operations:           b.Operations,
		DominantShapeKeys:    b.DominantShapeKeys,
	})
}

type AnalysisHotWindow struct {
	Start                time.Time      `json:"start"`
	End                  time.Time      `json:"end"`
	LoggedSlowOperations int            `json:"loggedSlowOperations"`
	CumulativeDurationMs int64          `json:"cumulativeDurationMs"`
	LatencyP95Ms         *int64         `json:"latencyP95Ms"`
	DominantOperations   map[string]int `json:"dominantOperations"`
	DominantShapeKeys    []string       `json:"dominantShapeKeys"`
}

func (w AnalysisHotWindow) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Start                string         `json:"start"`
		End                  string         `json:"end"`
		LoggedSlowOperations int            `json:"loggedSlowOperations"`
		CumulativeDurationMs int64          `json:"cumulativeDurationMs"`
		LatencyP95Ms         *int64         `json:"latencyP95Ms"`
		DominantOperations   map[string]int `json:"dominantOperations"`
		DominantShapeKeys    []string       `json:"dominantShapeKeys"`
	}{
		Start:                formatJSONTime(w.Start),
		End:                  formatJSONTime(w.End),
		LoggedSlowOperations: w.LoggedSlowOperations,
		CumulativeDurationMs: w.CumulativeDurationMs,
		LatencyP95Ms:         w.LatencyP95Ms,
		DominantOperations:   w.DominantOperations,
		DominantShapeKeys:    w.DominantShapeKeys,
	})
}

type AnalysisQueryShape struct {
	ShapeKey           string                             `json:"shapeKey"`
	Identity           ShapeIdentity                      `json:"identity"`
	Label              string                             `json:"label"`
	Namespace          string                             `json:"namespace"`
	Category           string                             `json:"category"`
	Operation          string                             `json:"operation"`
	ObservedOperations map[string]int                     `json:"observedOperations"`
	RedactedShape      RedactedShape                      `json:"redactedShape"`
	CursorCounts       AnalysisCursorCounts               `json:"cursorCounts"`
	LatencyMs          LatencySummary                     `json:"latencyMs"`
	Impact             AnalysisImpact                     `json:"impact"`
	Execution          map[string]OptionalMetricAggregate `json:"execution"`
	Storage            map[string]OptionalMetricAggregate `json:"storage"`
	Planner            AnalysisPlannerEvidence            `json:"planner"`
	PlanEvidence       AnalysisPlanEvidence               `json:"planEvidence"`
	Indicators         []string                           `json:"indicators"`
	Ratios             map[string]*float64                `json:"ratios"`
	WorstSample        AnalysisWorstSample                `json:"worstSample"`
	GetMoreAssociation string                             `json:"getMoreAssociation"`
	Truncated          bool                               `json:"truncated"`
}

type AnalysisCursorCounts struct {
	InitialOperations int64 `json:"initialOperations"`
	GetMoreOperations int64 `json:"getMoreOperations"`
	TotalOperations   int64 `json:"totalOperations"`
	InitialDurationMs int64 `json:"initialDurationMs"`
	GetMoreDurationMs int64 `json:"getMoreDurationMs"`
	TotalDurationMs   int64 `json:"totalDurationMs"`
}

type AnalysisImpact struct {
	TotalDurationMs              int64   `json:"totalDurationMs"`
	ShareOfLoggedSlowDurationPct float64 `json:"shareOfLoggedSlowDurationPct"`
	Rank                         int     `json:"rank"`
}

type AnalysisPlannerEvidence struct {
	HasSortStage              bool   `json:"hasSortStage"`
	HasSortStageEvaluated     int64  `json:"hasSortStageEvaluated"`
	HasSortStageObserved      int64  `json:"hasSortStageObserved"`
	FromMultiPlanner          bool   `json:"fromMultiPlanner"`
	FromMultiPlannerEvaluated int64  `json:"fromMultiPlannerEvaluated"`
	FromMultiPlannerObserved  int64  `json:"fromMultiPlannerObserved"`
	Replanned                 bool   `json:"replanned"`
	ReplannedEvaluated        int64  `json:"replannedEvaluated"`
	ReplannedObserved         int64  `json:"replannedObserved"`
	ReplanReason              string `json:"replanReason,omitempty"`
}

type AnalysisPlanEvidence struct {
	Variants         []AnalysisPlanVariant `json:"variants"`
	UnknownPlanCount int                   `json:"unknownPlanCount"`
}

type AnalysisPlanVariant struct {
	Summary        string `json:"summary"`
	PlanCacheKey   string `json:"planCacheKey"`
	QueryFramework string `json:"queryFramework"`
	Count          int    `json:"count"`
}

type AnalysisWorstSample struct {
	Timestamp   *string `json:"timestamp"`
	DurationMs  int64   `json:"durationMs"`
	SourceFile  string  `json:"sourceFile"`
	SourceLine  int64   `json:"sourceLine"`
	PlanSummary string  `json:"planSummary"`
}

type AnalysisNamespace struct {
	Name                         string                  `json:"name"`
	LoggedSlowOperations         int                     `json:"loggedSlowOperations"`
	CumulativeDurationMs         int64                   `json:"cumulativeDurationMs"`
	ShareOfLoggedSlowDurationPct float64                 `json:"shareOfLoggedSlowDurationPct"`
	P95Ms                        *int64                  `json:"p95Ms"`
	MaxMs                        *int64                  `json:"maxMs"`
	ShapeCount                   int                     `json:"shapeCount"`
	CollectionScanCount          int                     `json:"collectionScanCount"`
	DocsExamined                 OptionalMetricAggregate `json:"docsExamined"`
	KeysExamined                 OptionalMetricAggregate `json:"keysExamined"`
	TopShapeKeys                 []string                `json:"topShapeKeys"`
}

type AnalysisInternalOp struct {
	Operation       string `json:"operation"`
	Count           int    `json:"count"`
	TotalDurationMs int64  `json:"totalDurationMs"`
	MaxDurationMs   int64  `json:"maxDurationMs"`
}

type AnalysisOmitted struct {
	QueryShapes            int     `json:"queryShapes"`
	QueryOperations        int     `json:"queryOperations"`
	QueryDurationMs        int64   `json:"queryDurationMs"`
	QueryDurationSharePct  float64 `json:"queryDurationSharePct"`
	Namespaces             int     `json:"namespaces"`
	InternalOperationTypes int     `json:"internalOperationTypes"`
}

func computeAnalysis(files []string, discoveryFailures []FileFailure, cfg Config) (AnalysisReport, error) {
	cfg.JSON = true
	pass := newPassStats(files, discoveryFailures)
	shapeAggs := map[string]*shapeAgg{}
	internalAggs := map[string]*AnalysisInternalOp{}
	nsAggs := map[string]*namespaceAgg{}
	globalHist := DurationHistogram{}
	globalCategory := map[string]int{}
	globalOperation := map[string]int{}
	validLineBuckets := map[int64]int64{}
	buckets := map[int64]*analysisBucketAgg{}
	var slowFrom *time.Time
	var slowTo *time.Time
	var totalMs int64
	var accepted int64
	var internalCount int64

	parseFiles(files, parseOptions{
		From: cfg.From,
		To:   cfg.To,
		OnStructured: func(ts time.Time) {
			bucket := ts.UTC().Truncate(cfg.Bucket)
			validLineBuckets[bucket.UnixNano()]++
		},
	}, func(op *SlowOperation) {
		accepted++
		updateObservedRange(&slowFrom, &slowTo, op.Timestamp)
		globalHist.Add(op.Duration)
		totalMs += op.DurationMillis
		globalCategory[op.Category]++
		globalOperation[op.Operation]++

		bucketStart := op.Timestamp.UTC().Truncate(cfg.Bucket)
		bucket := buckets[bucketStart.UnixNano()]
		if bucket == nil {
			bucket = &analysisBucketAgg{start: bucketStart, hist: DurationHistogram{}, operations: map[string]int{}, shapeDuration: map[string]int64{}}
			buckets[bucketStart.UnixNano()] = bucket
		}
		bucket.count++
		bucket.durationMs += op.DurationMillis
		bucket.hist.Add(op.Duration)
		bucket.operations[op.Operation]++
		bucket.shapeDuration[op.ShapeKey] += op.DurationMillis

		if op.Internal {
			internalCount++
			rawName := sanitizeBoundedString(op.CommandName, 80)
			if rawName == "" {
				rawName = "internal"
			}
			ia := internalAggs[rawName]
			if ia == nil {
				ia = &AnalysisInternalOp{Operation: rawName}
				internalAggs[rawName] = ia
			}
			ia.Count++
			ia.TotalDurationMs += op.DurationMillis
			if op.DurationMillis > ia.MaxDurationMs {
				ia.MaxDurationMs = op.DurationMillis
			}
			if !cfg.IncludeInternal {
				return
			}
		}

		agg := shapeAggs[op.ShapeKey]
		if agg == nil {
			agg = newShapeAgg(op)
			shapeAggs[op.ShapeKey] = agg
		}
		agg.add(op)

		na := nsAggs[op.Namespace]
		if na == nil {
			na = &namespaceAgg{name: op.Namespace, hist: DurationHistogram{}, shapeKeys: map[string]int64{}}
			nsAggs[op.Namespace] = na
		}
		na.add(op)
	}, &pass)
	finalizeStats(&pass)

	if pass.InputObservedFrom != nil && pass.InputObservedTo != nil {
		for ts := pass.InputObservedFrom.UTC().Truncate(cfg.Bucket); !ts.After(pass.InputObservedTo.UTC()); ts = ts.Add(cfg.Bucket) {
			if _, ok := validLineBuckets[ts.UnixNano()]; !ok {
				validLineBuckets[ts.UnixNano()] = 0
			}
		}
	}

	selected := selectShapesByRanking(shapeAggs, effectiveTop(cfg, "analysis"), cfg.SortBy)
	selectedSet := map[string]struct{}{}
	for _, key := range selected {
		selectedSet[key] = struct{}{}
	}
	omittedShapes, omittedOps, omittedMs := omittedShapeTotals(shapeAggs, selectedSet)
	labels := uniqueShapeLabels(selected, shapeAggs)
	queryShapes := make([]AnalysisQueryShape, 0, len(selected))
	for rank, key := range selected {
		agg := shapeAggs[key]
		queryShapes = append(queryShapes, analysisShapeJSON(agg, labels[key], rank+1, totalMs))
	}

	timeline, err := analysisTimeline(cfg, buckets, validLineBuckets, pass.InputObservedFrom, pass.InputObservedTo, selectedSet)
	if err != nil {
		return AnalysisReport{}, err
	}
	hot := hotWindows(timeline)
	namespaces, omittedNamespaces := analysisNamespaces(nsAggs, totalMs, effectiveTop(cfg, "analysis"), selectedSet)
	internalOps, omittedInternal := analysisInternalOps(internalAggs)
	durationSeconds := analysisDurationSeconds(cfg, pass, Range{From: slowFrom, To: slowTo})
	indicators := analysisIndicators(shapeAggs)

	report := AnalysisReport{
		SchemaVersion: analysisSchema,
		Report:        "analysis",
		GeneratedAt:   formatJSONTime(time.Now()),
		Semantics: AnalysisSemantics{
			Dataset:                 "mongodb_diagnostic_logged_slow_operations",
			RepresentsTotalWorkload: false,
			CountsMayBeSampled:      true,
			DurationMeaning:         "cumulative operation duration; concurrent durations may overlap",
			Timezone:                "UTC",
			EmptyBucketMeaning:      "no accepted slow operation was observed in supplied logs",
			UntrustedInputSanitized: true,
			SlowOpConfiguration:     map[string]any{"thresholdMs": nil, "sampleRate": nil, "source": "unknown"},
		},
		Ranges: AnalysisRanges{
			Requested:               AnalysisRequestedRange{From: cfg.From, To: cfg.To, FromInclusive: true, ToExclusive: true},
			InputObserved:           AnalysisObservedRange{First: pass.InputObservedFrom, Last: pass.InputObservedTo},
			SlowOperationsObserved:  AnalysisObservedRange{First: slowFrom, Last: slowTo},
			AnalysisDurationSeconds: durationSeconds,
		},
		Options:     AnalysisOptions{Bucket: cfg.Bucket.String(), Top: effectiveTop(cfg, "analysis"), SortBy: cfg.SortBy, IncludeInternal: cfg.IncludeInternal, FillGaps: cfg.FillGaps, MaxBuckets: cfg.MaxBuckets},
		Diagnostics: analysisDiagnostics(pass.Diagnostics),
		Global: AnalysisGlobal{
			LoggedSlowOperations: accepted,
			CumulativeDurationMs: totalMs,
			LatencyMs:            globalHist.Summary(true),
			CountsByCategory:     sortedCountMap(globalCategory),
			CountsByOperation:    sortedCountMap(globalOperation),
			Indicators:           indicators,
			InternalCount:        internalCount,
			Rates:                analysisRates(accepted, durationSeconds),
		},
		Timeline:           timeline,
		HotWindows:         hot,
		QueryShapes:        queryShapes,
		Namespaces:         namespaces,
		InternalOperations: internalOps,
		Omitted: AnalysisOmitted{
			QueryShapes:            omittedShapes,
			QueryOperations:        omittedOps,
			QueryDurationMs:        omittedMs,
			QueryDurationSharePct:  percentOf(omittedMs, totalMs),
			Namespaces:             omittedNamespaces,
			InternalOperationTypes: omittedInternal,
		},
		IndicatorThresholds: map[string]any{"highScanAmplificationRatio": 100, "highScanAmplificationMinDocsExamined": 1000},
	}
	if err := validateAnalysisReport(report, selectedSet); err != nil {
		return AnalysisReport{}, err
	}
	return report, nil
}

type analysisBucketAgg struct {
	start         time.Time
	count         int
	durationMs    int64
	hist          DurationHistogram
	operations    map[string]int
	shapeDuration map[string]int64
}

type namespaceAgg struct {
	name            string
	count           int
	durationMs      int64
	hist            DurationHistogram
	shapeKeys       map[string]int64
	collectionScans int
	keys            metricAggState
	docs            metricAggState
}

func (n *namespaceAgg) add(op *SlowOperation) {
	n.count++
	n.durationMs += op.DurationMillis
	n.hist.Add(op.Duration)
	n.shapeKeys[op.ShapeKey] += op.DurationMillis
	if op.PlanSummary == "COLLSCAN" {
		n.collectionScans++
	}
	n.keys.Add(op.KeysExamined)
	n.docs.Add(op.DocsExamined)
}

func analysisDiagnostics(d Diagnostics) AnalysisDiagnostics {
	return AnalysisDiagnostics{
		InputFilesDiscovered:      d.FilesDiscovered,
		InputFilesRead:            d.FilesRead,
		InputFilesFailed:          d.FilesFailed,
		LinesRead:                 d.LinesRead,
		ValidStructuredLines:      d.ValidStructuredLines,
		SlowQueryLines:            d.SlowQueryLines,
		AcceptedSlowOperations:    d.AcceptedSlowOperations,
		NonSlowStructuredLines:    d.NonSlowStructuredLines,
		FilteredByTime:            d.FilteredByTime,
		FilteredByDuration:        d.FilteredByDuration,
		MalformedLines:            d.LinesMalformed,
		RejectedSlowOperations:    d.RejectedSlowOperations,
		RejectedByReason:          sortedInt64Map(d.RejectedByReason),
		TruncatedSlowOperations:   d.TruncatedSlowOperations,
		CanonicalizationFallbacks: d.CanonicalizationFallbacks,
		CanonicalizationFailures:  d.CanonicalizationFailures,
		UnassociatedGetMore:       d.UnassociatedGetMore,
		PartialInput:              d.PartialInput,
		InputStatus:               d.InputStatus,
	}
}

func analysisDurationSeconds(cfg Config, stats PassStats, slowRange Range) int64 {
	if cfg.From != nil && cfg.To != nil {
		return int64(cfg.To.Sub(*cfg.From).Seconds())
	}
	from, to := stats.InputObservedFrom, stats.InputObservedTo
	if from == nil || to == nil {
		from, to = slowRange.From, slowRange.To
	}
	if from == nil || to == nil {
		return 0
	}
	return int64(to.Sub(*from).Seconds())
}

func analysisTimeline(cfg Config, buckets map[int64]*analysisBucketAgg, valid map[int64]int64, inputFrom, inputTo *time.Time, emitted map[string]struct{}) ([]AnalysisBucket, error) {
	keys := make([]int64, 0, len(buckets))
	if cfg.FillGaps {
		start, end := selectedTimelineBounds(cfg, inputFrom, inputTo, buckets)
		if start != nil && end != nil {
			count := int(end.Sub(*start)/cfg.Bucket) + 1
			if count > cfg.MaxBuckets {
				return nil, fmt.Errorf("analysis timeline would emit %d buckets; increase --bucket or --max-buckets", count)
			}
			for ts := *start; !ts.After(*end); ts = ts.Add(cfg.Bucket) {
				keys = append(keys, ts.UnixNano())
			}
		}
	} else {
		for key := range buckets {
			keys = append(keys, key)
		}
	}
	if !cfg.FillGaps {
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	} else {
		seen := map[int64]struct{}{}
		compact := keys[:0]
		for _, key := range keys {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			compact = append(compact, key)
		}
		keys = compact
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	}
	rows := make([]AnalysisBucket, 0, len(keys))
	for _, key := range keys {
		agg := buckets[key]
		start := time.Unix(0, key).UTC()
		row := AnalysisBucket{Start: start, End: start.Add(cfg.Bucket), ValidLogLines: valid[key], Operations: operationZeroMap(), DominantShapeKeys: []string{}}
		if agg != nil {
			row.LoggedSlowOperations = agg.count
			row.CumulativeDurationMs = agg.durationMs
			if p95 := agg.hist.Summary(false).P95; p95 != nil {
				row.LatencyP95Ms = p95
			}
			for op, count := range agg.operations {
				row.Operations[op] = count
			}
			row.DominantShapeKeys = topShapeKeys(agg.shapeDuration, 5, emitted)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func selectedTimelineBounds(cfg Config, inputFrom, inputTo *time.Time, buckets map[int64]*analysisBucketAgg) (*time.Time, *time.Time) {
	var start *time.Time
	var end *time.Time
	if cfg.From != nil {
		s := cfg.From.UTC().Truncate(cfg.Bucket)
		start = &s
	} else if inputFrom != nil {
		s := inputFrom.UTC().Truncate(cfg.Bucket)
		start = &s
	}
	if cfg.To != nil {
		e := cfg.To.Add(-time.Nanosecond).UTC().Truncate(cfg.Bucket)
		end = &e
	} else if inputTo != nil {
		e := inputTo.UTC().Truncate(cfg.Bucket)
		end = &e
	}
	if start == nil || end == nil {
		for key := range buckets {
			ts := time.Unix(0, key).UTC()
			if start == nil || ts.Before(*start) {
				v := ts
				start = &v
			}
			if end == nil || ts.After(*end) {
				v := ts
				end = &v
			}
		}
	}
	return start, end
}

func operationZeroMap() map[string]int {
	return map[string]int{"find": 0, "aggregate": 0, "count": 0, "distinct": 0, "insert": 0, "update": 0, "delete": 0, "findAndModify": 0, "bulkWrite": 0, "getMore": 0, "command": 0, "internal": 0, "other": 0}
}

func hotWindows(timeline []AnalysisBucket) []AnalysisHotWindow {
	items := append([]AnalysisBucket(nil), timeline...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].CumulativeDurationMs != items[j].CumulativeDurationMs {
			return items[i].CumulativeDurationMs > items[j].CumulativeDurationMs
		}
		if items[i].LoggedSlowOperations != items[j].LoggedSlowOperations {
			return items[i].LoggedSlowOperations > items[j].LoggedSlowOperations
		}
		return items[i].Start.Before(items[j].Start)
	})
	if len(items) > 10 {
		items = items[:10]
	}
	out := make([]AnalysisHotWindow, 0, len(items))
	for _, item := range items {
		if item.LoggedSlowOperations == 0 {
			continue
		}
		out = append(out, AnalysisHotWindow{Start: item.Start, End: item.End, LoggedSlowOperations: item.LoggedSlowOperations, CumulativeDurationMs: item.CumulativeDurationMs, LatencyP95Ms: item.LatencyP95Ms, DominantOperations: item.Operations, DominantShapeKeys: item.DominantShapeKeys})
	}
	return out
}

func analysisShapeJSON(agg *shapeAgg, label string, rank int, totalMs int64) AnalysisQueryShape {
	summary := agg.hist.Summary(true)
	plans := planVariants(agg.planVariants)
	indicators := indicatorList(agg.indicators)
	planner := AnalysisPlannerEvidence{
		HasSortStage:              agg.planner.hasSortStageObserved > 0,
		HasSortStageEvaluated:     agg.planner.hasSortStageEvaluated,
		HasSortStageObserved:      agg.planner.hasSortStageObserved,
		FromMultiPlanner:          agg.planner.fromMultiPlannerObserved > 0,
		FromMultiPlannerEvaluated: agg.planner.fromMultiPlannerEvaluated,
		FromMultiPlannerObserved:  agg.planner.fromMultiPlannerObserved,
		Replanned:                 agg.planner.replannedObserved > 0,
		ReplannedEvaluated:        agg.planner.replannedEvaluated,
		ReplannedObserved:         agg.planner.replannedObserved,
		ReplanReason:              agg.planner.replanReason,
	}
	if len(plans) > 1 {
		indicators = appendUniqueIndicator(indicators, "multiple_plan_variants_observed")
	}
	return AnalysisQueryShape{
		ShapeKey:           analysisShapeKey(agg.meta.namespace, agg.meta.operation, agg.meta.identity),
		Identity:           agg.meta.identity,
		Label:              label,
		Namespace:          agg.meta.namespace,
		Category:           agg.meta.category,
		Operation:          agg.meta.operation,
		ObservedOperations: sortedCountMap(agg.observedOps),
		RedactedShape:      agg.meta.redactedShape,
		CursorCounts:       AnalysisCursorCounts{InitialOperations: agg.initialCount, GetMoreOperations: agg.getMoreCount, TotalOperations: int64(agg.count), InitialDurationMs: agg.initialDurationMs, GetMoreDurationMs: agg.getMoreDurationMs, TotalDurationMs: agg.hist.TotalMs()},
		LatencyMs:          summary,
		Impact:             AnalysisImpact{TotalDurationMs: agg.hist.TotalMs(), ShareOfLoggedSlowDurationPct: percentOf(agg.hist.TotalMs(), totalMs), Rank: rank},
		Execution: map[string]OptionalMetricAggregate{
			"keysExamined": agg.metrics.KeysExamined.JSON(), "docsExamined": agg.metrics.DocsExamined.JSON(), "nreturned": agg.metrics.NReturned.JSON(), "nMatched": agg.metrics.NMatched.JSON(), "nModified": agg.metrics.NModified.JSON(), "nInserted": agg.metrics.NInserted.JSON(), "nDeleted": agg.metrics.NDeleted.JSON(), "numYields": agg.metrics.NumYields.JSON(), "cpuNanos": agg.metrics.CPUNanos.JSON(),
		},
		Storage: map[string]OptionalMetricAggregate{
			"bytesRead": agg.metrics.BytesRead.JSON(), "bytesWritten": agg.metrics.BytesWritten.JSON(), "timeReadingMicros": agg.metrics.TimeReadingMicros.JSON(), "timeWritingMicros": agg.metrics.TimeWritingMicros.JSON(), "lockWaitMicros": agg.metrics.LockWaitMicros.JSON(),
		},
		Planner:            planner,
		PlanEvidence:       AnalysisPlanEvidence{Variants: plans, UnknownPlanCount: agg.unknownPlanCount},
		Indicators:         indicators,
		Ratios:             ratioMap(agg),
		WorstSample:        worstSample(agg.worst),
		GetMoreAssociation: agg.meta.getMoreAssoc,
		Truncated:          agg.meta.truncated,
	}
}

func worstSample(op *SlowOperation) AnalysisWorstSample {
	if op == nil {
		return AnalysisWorstSample{}
	}
	ts := formatJSONTime(op.Timestamp)
	return AnalysisWorstSample{Timestamp: &ts, DurationMs: op.DurationMillis, SourceFile: filepath.Base(op.SourceFile), SourceLine: op.SourceLine, PlanSummary: op.PlanSummary}
}

func planVariants(values map[string]int) []AnalysisPlanVariant {
	out := make([]AnalysisPlanVariant, 0, len(values))
	for key, count := range values {
		parts := strings.Split(key, "\x00")
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		out = append(out, AnalysisPlanVariant{Summary: parts[0], PlanCacheKey: parts[1], QueryFramework: parts[2], Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Summary != out[j].Summary {
			return out[i].Summary < out[j].Summary
		}
		if out[i].PlanCacheKey != out[j].PlanCacheKey {
			return out[i].PlanCacheKey < out[j].PlanCacheKey
		}
		return out[i].QueryFramework < out[j].QueryFramework
	})
	return out
}

func indicatorList(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func appendUniqueIndicator(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	values = append(values, value)
	sort.Strings(values)
	return values
}

func ratioMap(agg *shapeAgg) map[string]*float64 {
	return map[string]*float64{
		"readDocsPerReturned":     metricRatio(agg.metrics.DocsExamined, agg.metrics.NReturned),
		"readKeysPerReturned":     metricRatio(agg.metrics.KeysExamined, agg.metrics.NReturned),
		"writeDocsPerMatched":     metricRatio(agg.metrics.DocsExamined, agg.metrics.NMatched),
		"storageReadTimeSharePct": metricPctRatio(agg.metrics.TimeReadingMicros, agg.hist.TotalMs()*1000),
		"lockWaitTimeSharePct":    metricPctRatio(agg.metrics.LockWaitMicros, agg.hist.TotalMs()*1000),
	}
}

func metricRatio(num, den metricAggState) *float64 {
	if num.present == 0 || den.present == 0 || den.sum <= 0 {
		return nil
	}
	v := float64(num.sum) / float64(den.sum)
	return &v
}

func metricPctRatio(num metricAggState, den int64) *float64 {
	if num.present == 0 || den <= 0 {
		return nil
	}
	v := 100 * float64(num.sum) / float64(den)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	return &v
}

func percentOf(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return 100 * float64(num) / float64(den)
}

func topShapeKeys(values map[string]int64, limit int, emitted map[string]struct{}) []string {
	type item struct {
		id    string
		value int64
	}
	items := make([]item, 0, len(values))
	for id, value := range values {
		if _, ok := emitted[id]; !ok {
			continue
		}
		if id == "" {
			continue
		}
		items = append(items, item{id: id, value: value})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].value != items[j].value {
			return items[i].value > items[j].value
		}
		return items[i].id < items[j].id
	})
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.id)
	}
	return out
}

func analysisNamespaces(values map[string]*namespaceAgg, totalMs int64, top int, emitted map[string]struct{}) ([]AnalysisNamespace, int) {
	items := make([]*namespaceAgg, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].durationMs != items[j].durationMs {
			return items[i].durationMs > items[j].durationMs
		}
		return items[i].name < items[j].name
	})
	omitted := 0
	if top > 0 && len(items) > top {
		omitted = len(items) - top
		items = items[:top]
	}
	out := make([]AnalysisNamespace, 0, len(items))
	for _, item := range items {
		s := item.hist.Summary(false)
		out = append(out, AnalysisNamespace{Name: item.name, LoggedSlowOperations: item.count, CumulativeDurationMs: item.durationMs, ShareOfLoggedSlowDurationPct: percentOf(item.durationMs, totalMs), P95Ms: s.P95, MaxMs: s.Max, ShapeCount: len(item.shapeKeys), CollectionScanCount: item.collectionScans, DocsExamined: item.docs.JSON(), KeysExamined: item.keys.JSON(), TopShapeKeys: topShapeKeys(item.shapeKeys, 5, emitted)})
	}
	return out, omitted
}

func validateAnalysisReport(report AnalysisReport, emitted map[string]struct{}) error {
	d := report.Diagnostics
	if d.ValidStructuredLines != d.NonSlowStructuredLines+d.SlowQueryLines {
		return fmt.Errorf("internal accounting error: validStructuredLines=%d nonSlowStructuredLines=%d slowQueryLines=%d", d.ValidStructuredLines, d.NonSlowStructuredLines, d.SlowQueryLines)
	}
	classifiedSlow := d.AcceptedSlowOperations + d.RejectedSlowOperations + d.FilteredByTime + d.FilteredByDuration
	if d.SlowQueryLines != classifiedSlow {
		return fmt.Errorf("internal accounting error: slowQueryLines=%d classifiedSlow=%d", d.SlowQueryLines, classifiedSlow)
	}
	var rejectedTotal int64
	for _, count := range d.RejectedByReason {
		rejectedTotal += count
	}
	if d.RejectedSlowOperations != rejectedTotal {
		return fmt.Errorf("internal accounting error: rejectedSlowOperations=%d rejectedByReasonSum=%d", d.RejectedSlowOperations, rejectedTotal)
	}

	seen := map[string]struct{}{}
	for _, shape := range report.QueryShapes {
		if shape.ShapeKey == "" {
			return fmt.Errorf("internal reference error: emitted query shape has empty shapeKey")
		}
		if _, ok := seen[shape.ShapeKey]; ok {
			return fmt.Errorf("internal reference error: duplicate shapeKey %q", shape.ShapeKey)
		}
		seen[shape.ShapeKey] = struct{}{}
		if _, ok := emitted[shape.ShapeKey]; !ok {
			return fmt.Errorf("internal reference error: query shape %q was not selected", shape.ShapeKey)
		}
	}
	for _, bucket := range report.Timeline {
		for _, key := range bucket.DominantShapeKeys {
			if _, ok := seen[key]; !ok {
				return fmt.Errorf("internal reference error: timeline references omitted shapeKey %q", key)
			}
		}
	}
	for _, window := range report.HotWindows {
		for _, key := range window.DominantShapeKeys {
			if _, ok := seen[key]; !ok {
				return fmt.Errorf("internal reference error: hot window references omitted shapeKey %q", key)
			}
		}
	}
	for _, ns := range report.Namespaces {
		for _, key := range ns.TopShapeKeys {
			if _, ok := seen[key]; !ok {
				return fmt.Errorf("internal reference error: namespace references omitted shapeKey %q", key)
			}
		}
	}
	return nil
}

func analysisInternalOps(values map[string]*AnalysisInternalOp) ([]AnalysisInternalOp, int) {
	items := make([]AnalysisInternalOp, 0, len(values))
	for _, value := range values {
		items = append(items, *value)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Operation < items[j].Operation
	})
	omitted := 0
	if len(items) > 20 {
		omitted = len(items) - 20
		items = items[:20]
	}
	return items, omitted
}
