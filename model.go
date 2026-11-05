package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	maxLogicalLineBytes = 16 << 20
	schemaVersion       = 1
	analysisSchema      = 2
)

type OptionalInt64 struct {
	Value int64
	Valid bool
}

type OptionalBool struct {
	Value bool
	Valid bool
}

type ShapeIdentity struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	PlanCacheKey string `json:"planCacheKey,omitempty"`
}

type OperationInfo struct {
	Category  string `json:"category"`
	Operation string `json:"operation"`
	Internal  bool   `json:"internal"`
}

type RedactedShape struct {
	Command          string         `json:"command"`
	Filter           any            `json:"filter,omitempty"`
	Sort             any            `json:"sort,omitempty"`
	ProjectionFields []string       `json:"projectionFields,omitempty"`
	Pipeline         any            `json:"pipeline,omitempty"`
	PipelineStages   []string       `json:"pipelineStages,omitempty"`
	Update           any            `json:"update,omitempty"`
	Hint             any            `json:"hint,omitempty"`
	Collation        any            `json:"collation,omitempty"`
	ReadPreference   any            `json:"readPreference,omitempty"`
	ReadConcern      any            `json:"readConcern,omitempty"`
	WriteConcern     any            `json:"writeConcern,omitempty"`
	AllowDiskUse     *bool          `json:"allowDiskUse,omitempty"`
	Extra            map[string]any `json:"extra,omitempty"`
}

type SlowOperation struct {
	Timestamp          time.Time
	Ctx                string
	Remote             string
	Namespace          string
	Category           string
	Operation          string
	OriginCategory     string
	OriginOperation    string
	CommandName        string
	Internal           bool
	Duration           time.Duration
	DurationMillis     int64
	PlanSummary        string
	ShapeKey           string
	ShapeID            string
	ShapeLabel         string
	QueryHashSource    string
	ShapeIdentity      ShapeIdentity
	RedactedShape      RedactedShape
	KeysExamined       OptionalInt64
	DocsExamined       OptionalInt64
	NReturned          OptionalInt64
	NMatched           OptionalInt64
	NModified          OptionalInt64
	NInserted          OptionalInt64
	NDeleted           OptionalInt64
	NumYields          OptionalInt64
	BytesRead          OptionalInt64
	BytesWritten       OptionalInt64
	TimeReadingMicros  OptionalInt64
	TimeWritingMicros  OptionalInt64
	PlanningTimeMicros OptionalInt64
	LockWaitMicros     OptionalInt64
	CPUNanos           OptionalInt64
	HasSortStage       OptionalBool
	UsedDisk           OptionalBool
	FromMultiPlanner   OptionalBool
	Replanned          OptionalBool
	ReplanReason       string
	PlanCacheKey       string
	QueryFramework     string
	GetMoreAssociated  bool
	Truncated          bool
	SourceFile         string
	SourceLine         int64
}

type Diagnostics struct {
	InputStatus                    string           `json:"inputStatus"`
	FilesDiscovered                int              `json:"filesDiscovered"`
	FilesRead                      int              `json:"filesRead"`
	FilesFailed                    int              `json:"filesFailed"`
	FilesSkipped                   int              `json:"filesSkipped,omitempty"`
	LinesRead                      int64            `json:"linesRead"`
	LinesMalformed                 int64            `json:"linesMalformed"`
	RecordsRejected                int64            `json:"recordsRejected"`
	RecordsFiltered                int64            `json:"recordsFiltered"`
	ShapeCanonicalizationTruncated int64            `json:"shapeCanonicalizationTruncated"`
	ValidStructuredLines           int64            `json:"validStructuredLines,omitempty"`
	NonSlowStructuredLines         int64            `json:"nonSlowStructuredLines,omitempty"`
	SlowQueryLines                 int64            `json:"slowQueryLines,omitempty"`
	AcceptedSlowOperations         int64            `json:"acceptedSlowOperations,omitempty"`
	FilteredByTime                 int64            `json:"filteredByTime,omitempty"`
	FilteredByDuration             int64            `json:"filteredByDuration,omitempty"`
	RejectedSlowOperations         int64            `json:"rejectedSlowOperations,omitempty"`
	RejectedByReason               map[string]int64 `json:"rejectedByReason,omitempty"`
	TruncatedSlowOperations        int64            `json:"truncatedSlowOperations,omitempty"`
	CanonicalizationFallbacks      int64            `json:"canonicalizationFallbacks,omitempty"`
	CanonicalizationFailures       int64            `json:"canonicalizationFailures,omitempty"`
	UnassociatedGetMore            int64            `json:"unassociatedGetMore,omitempty"`
	PartialInput                   bool             `json:"partialInput,omitempty"`
	UnknownOperationTypes          int64            `json:"unknownOperationTypes,omitempty"`
}

type FileFailure struct {
	Path   string
	Reason string
}

type PassStats struct {
	Diagnostics       Diagnostics
	Failures          []FileFailure
	InputFiles        []string
	Skipped           []string
	InputObservedFrom *time.Time
	InputObservedTo   *time.Time
}

type Range struct {
	From *time.Time `json:"from"`
	To   *time.Time `json:"to"`
}

func (r Range) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		From *string `json:"from"`
		To   *string `json:"to"`
	}{
		From: formatJSONTimePtr(r.From),
		To:   formatJSONTimePtr(r.To),
	})
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

type OptionalMetricAggregate struct {
	PresentCount int64    `json:"presentCount"`
	Sum          int64    `json:"sum"`
	Avg          *float64 `json:"avg"`
	Max          *int64   `json:"max"`
}

type metricAggState struct {
	present int64
	sum     int64
	max     int64
}

func (m *metricAggState) Add(v OptionalInt64) {
	if !v.Valid {
		return
	}
	if m.present == 0 || v.Value > m.max {
		m.max = v.Value
	}
	m.present++
	m.sum += v.Value
}

func (m metricAggState) JSON() OptionalMetricAggregate {
	out := OptionalMetricAggregate{PresentCount: m.present, Sum: m.sum}
	if m.present > 0 {
		avg := float64(m.sum) / float64(m.present)
		max := m.max
		out.Avg = &avg
		out.Max = &max
	}
	return out
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

func (h *DurationHistogram) TotalMs() int64 {
	return int64(h.totalMs)
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
	trimmed := shapeID
	if trimmed == "" {
		trimmed = stableHash(op, ns, "missing-shape")
	}
	if len(trimmed) > 12 {
		trimmed = trimmed[:12]
	}
	return fmt.Sprintf("%s:%s#%s", op, ns, trimmed)
}

func stableHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func analysisShapeKey(namespace, operation string, identity ShapeIdentity) string {
	return stableHash(namespace, operation, identity.Source, identity.ID)
}

func formatJSONTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func formatJSONTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	value := formatJSONTime(*t)
	return &value
}

func sanitizeBoundedString(value string, max int) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}
