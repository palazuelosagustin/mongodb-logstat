package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	maxLogicalLineBytes = 16 << 20
	schemaVersion       = 1
)

type OptionalInt64 struct {
	Value int64
	Valid bool
}

type SlowOperation struct {
	Timestamp        time.Time
	Namespace        string
	Operation        string
	CommandName      string
	Duration         time.Duration
	PlanSummary      string
	ShapeKey         string
	ShapeID          string
	ShapeLabel       string
	QueryHashSource  string
	KeysExamined     OptionalInt64
	DocsExamined     OptionalInt64
	NReturned        OptionalInt64
	NumYields        OptionalInt64
	HasSortStage     bool
	FromMultiPlanner bool
	Truncated        bool
	SourceFile       string
	SourceLine       int64
}

type Diagnostics struct {
	InputStatus                    string `json:"inputStatus"`
	FilesDiscovered                int    `json:"filesDiscovered"`
	FilesRead                      int    `json:"filesRead"`
	FilesFailed                    int    `json:"filesFailed"`
	LinesRead                      int64  `json:"linesRead"`
	LinesMalformed                 int64  `json:"linesMalformed"`
	RecordsRejected                int64  `json:"recordsRejected"`
	RecordsFiltered                int64  `json:"recordsFiltered"`
	ShapeCanonicalizationTruncated int64  `json:"shapeCanonicalizationTruncated"`
}

type FileFailure struct {
	Path   string
	Reason string
}

type PassStats struct {
	Diagnostics Diagnostics
	Failures    []FileFailure
}

type Range struct {
	From *time.Time `json:"from"`
	To   *time.Time `json:"to"`
}

type DurationHistogram struct {
	totalCount uint64
	totalMs    float64
	minMs      int64
	maxMs      int64
	counts     map[int64]uint64
}

type LatencySummary struct {
	Min         *int64   `json:"min"`
	Avg         *float64 `json:"avg"`
	P50         *int64   `json:"p50"`
	P95         *int64   `json:"p95"`
	P99         *int64   `json:"p99"`
	Max         *int64   `json:"max"`
	Approximate bool     `json:"approximate"`
}

func (h *DurationHistogram) Add(d time.Duration) {
	ms := durationMillis(d)
	if h.counts == nil {
		h.counts = make(map[int64]uint64)
		h.minMs = ms
		h.maxMs = ms
	}
	if h.totalCount == 0 || ms < h.minMs {
		h.minMs = ms
	}
	if h.totalCount == 0 || ms > h.maxMs {
		h.maxMs = ms
	}
	h.counts[ms]++
	h.totalCount++
	h.totalMs += float64(d) / float64(time.Millisecond)
}

func (h *DurationHistogram) Count() uint64 {
	return h.totalCount
}

func (h *DurationHistogram) Summary(includeP99 bool) LatencySummary {
	if h.totalCount == 0 {
		return LatencySummary{Approximate: true}
	}

	min := h.minMs
	max := h.maxMs
	avg := h.totalMs / float64(h.totalCount)
	p50 := h.percentile(0.50)
	p95 := h.percentile(0.95)
	out := LatencySummary{
		Min:         &min,
		Avg:         &avg,
		P50:         &p50,
		P95:         &p95,
		Max:         &max,
		Approximate: true,
	}
	if includeP99 {
		p99 := h.percentile(0.99)
		out.P99 = &p99
	}
	return out
}

func (h *DurationHistogram) percentile(p float64) int64 {
	if h.totalCount == 0 {
		return 0
	}
	keys := make([]int64, 0, len(h.counts))
	for ms := range h.counts {
		keys = append(keys, ms)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	target := uint64(float64(h.totalCount)*p + 0.999999999)
	if target == 0 {
		target = 1
	}

	var seen uint64
	for _, ms := range keys {
		seen += h.counts[ms]
		if seen >= target {
			return ms
		}
	}
	return keys[len(keys)-1]
}

func durationMillis(d time.Duration) int64 {
	return int64(float64(d) / float64(time.Millisecond))
}

func shapeLabel(op, ns, shapeID string) string {
	if shapeID == "" {
		return fmt.Sprintf("%s:%s#null", op, ns)
	}
	trimmed := shapeID
	if len(trimmed) > 12 {
		trimmed = trimmed[:12]
	}
	return fmt.Sprintf("%s:%s#%s", op, ns, trimmed)
}

func stableHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
