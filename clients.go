package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

var clientOperationColumns = []string{"find", "aggregate", "count", "distinct", "insert", "update", "delete", "findAndModify", "bulkWrite", "getMore", "command", "internal", "other"}

type clientConnectionInfo struct {
	Ctx                    string
	ConnectionID           string
	Remote                 string
	ClientIP               string
	ApplicationName        string
	DriverName             string
	DriverVersion          string
	OSType                 string
	OSName                 string
	OSVersion              string
	PrincipalName          string
	AuthenticationDatabase string
	Mechanism              string
}

type clientAgg struct {
	User                   string
	AuthenticationDatabase string
	ClientIP               string
	Applications           map[string]struct{}
	Drivers                map[string]struct{}
	Remotes                map[string]struct{}
	ConnectionIDs          map[string]struct{}
	Operations             map[string]int
	Namespaces             map[string]int
	Samples                map[string]struct{}
	Hist                   DurationHistogram
	DocsExamined           int64
	Count                  int
	DurationMs             int64
	BestRank               int
}

type ClientsReport struct {
	Observed                       Range
	Stats                          PassStats
	Rows                           []clientsRowJSON
	Accepted                       int
	Ranking                        string
	TopLimit                       int
	DisplayedGroups                int
	OmittedGroups                  int
	OmittedSlowOps                 int
	OmittedCumulativeDurationMs    int64
	UncorrelatedSlowOperations     int
	UncorrelatedCumulativeDuration int64
}

type clientsReportJSON struct {
	reportEnvelope
	Ranking string             `json:"ranking"`
	Top     int                `json:"top"`
	Totals  clientsTotalsJSON  `json:"totals"`
	Rows    []clientsRowJSON   `json:"rows"`
	Omitted clientsOmittedJSON `json:"omitted"`
}

type clientsTotalsJSON struct {
	LoggedSlowOperations       int   `json:"loggedSlowOperations"`
	CorrelatedSlowOperations   int   `json:"correlatedSlowOperations"`
	UncorrelatedSlowOperations int   `json:"uncorrelatedSlowOperations"`
	CumulativeDurationMs       int64 `json:"cumulativeDurationMs"`
}

type clientsOmittedJSON struct {
	Groups                         int   `json:"groups"`
	SlowOperations                 int   `json:"slowOperations"`
	CumulativeDurationMs           int64 `json:"cumulativeDurationMs"`
	UncorrelatedSlowOperations     int   `json:"uncorrelatedSlowOperations"`
	UncorrelatedCumulativeDuration int64 `json:"uncorrelatedCumulativeDurationMs"`
}

type clientsRowJSON struct {
	User                   string           `json:"user"`
	AuthenticationDatabase string           `json:"authenticationDatabase"`
	ClientIP               string           `json:"clientIP"`
	Applications           []string         `json:"applications"`
	Drivers                []string         `json:"drivers"`
	Remotes                []string         `json:"remotes,omitempty"`
	LoggedSlowOperations   int              `json:"loggedSlowOperations"`
	CumulativeDurationMs   int64            `json:"cumulativeDurationMs"`
	LatencyMs              LatencySummary   `json:"latencyMs"`
	CountsByOperation      map[string]int   `json:"countsByOperation"`
	TopNamespaces          []summaryTopItem `json:"topNamespaces"`
	SampleLines            []string         `json:"sampleLines"`
	terminalRank           int
}

func computeClients(files []string, discoveryFailures []FileFailure, cfg Config) ClientsReport {
	stats := newPassStats(files, discoveryFailures)
	connections := map[string]*clientConnectionInfo{}
	aggs := map[string]*clientAgg{}
	accepted := 0
	var uncorrelatedOps int
	var uncorrelatedDurationMs int64
	var observedFrom *time.Time
	var observedTo *time.Time

	parseFiles(files, parseOptions{
		From: cfg.From,
		To:   cfg.To,
		OnStructuredEvent: func(event StructuredEvent) {
			captureClientMetadata(event.Raw, connections)
		},
	}, func(op *SlowOperation) {
		accepted++
		updateObservedRange(&observedFrom, &observedTo, op.Timestamp)

		conn := connections[op.Ctx]
		if op.Ctx == "" || conn == nil {
			uncorrelatedOps++
			uncorrelatedDurationMs += op.DurationMillis
			return
		}

		user := conn.PrincipalName
		if user == "" {
			user = "<unknown>"
		}
		authDB := conn.AuthenticationDatabase
		if authDB == "" {
			authDB = "<unknown>"
		}
		remote := firstNonEmpty(conn.Remote, op.Remote)
		clientIP := firstNonEmpty(conn.ClientIP, clientIPFromRemote(remote))
		if clientIP == "" {
			clientIP = "<unknown>"
		}

		key := user + "\x00" + authDB + "\x00" + clientIP
		agg := aggs[key]
		if agg == nil {
			agg = &clientAgg{
				User:                   user,
				AuthenticationDatabase: authDB,
				ClientIP:               clientIP,
				Applications:           map[string]struct{}{},
				Drivers:                map[string]struct{}{},
				Remotes:                map[string]struct{}{},
				ConnectionIDs:          map[string]struct{}{},
				Operations:             map[string]int{},
				Namespaces:             map[string]int{},
				Samples:                map[string]struct{}{},
			}
			aggs[key] = agg
		}
		agg.add(op, conn, remote)
	}, &stats)

	selected := selectClientAggsByRanking(aggs, effectiveTop(cfg, "clients"), clientsSortBy(cfg))
	selectedSet := map[string]struct{}{}
	for rank, key := range selected {
		selectedSet[key] = struct{}{}
		aggs[key].BestRank = rank + 1
	}

	rows := make([]clientsRowJSON, 0, len(selected))
	for _, key := range selected {
		rows = append(rows, clientAggRow(aggs[key]))
	}

	omittedGroups, omittedOps, omittedDurationMs := omittedClientTotals(aggs, selectedSet)
	finalizeStats(&stats)
	return ClientsReport{
		Observed:                       Range{From: observedFrom, To: observedTo},
		Stats:                          stats,
		Rows:                           rows,
		Accepted:                       accepted,
		Ranking:                        clientsSortBy(cfg),
		TopLimit:                       effectiveTop(cfg, "clients"),
		DisplayedGroups:                len(rows),
		OmittedGroups:                  omittedGroups,
		OmittedSlowOps:                 omittedOps,
		OmittedCumulativeDurationMs:    omittedDurationMs,
		UncorrelatedSlowOperations:     uncorrelatedOps,
		UncorrelatedCumulativeDuration: uncorrelatedDurationMs,
	}
}

func renderClients(cfg Config, report ClientsReport) (string, int, error) {
	if cfg.JSON {
		var displayedOps int
		var displayedMs int64
		for _, row := range report.Rows {
			displayedOps += row.LoggedSlowOperations
			displayedMs += row.CumulativeDurationMs
		}
		body := clientsReportJSON{
			reportEnvelope: newEnvelope("clients", cfg, report.Observed, report.Stats.Diagnostics),
			Ranking:        report.Ranking,
			Top:            report.TopLimit,
			Totals: clientsTotalsJSON{
				LoggedSlowOperations:       report.Accepted,
				CorrelatedSlowOperations:   displayedOps + report.OmittedSlowOps,
				UncorrelatedSlowOperations: report.UncorrelatedSlowOperations,
				CumulativeDurationMs:       displayedMs + report.OmittedCumulativeDurationMs + report.UncorrelatedCumulativeDuration,
			},
			Rows: report.Rows,
			Omitted: clientsOmittedJSON{
				Groups:                         report.OmittedGroups,
				SlowOperations:                 report.OmittedSlowOps,
				CumulativeDurationMs:           report.OmittedCumulativeDurationMs,
				UncorrelatedSlowOperations:     report.UncorrelatedSlowOperations,
				UncorrelatedCumulativeDuration: report.UncorrelatedCumulativeDuration,
			},
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	writeTerminalMetadata(&buf, cfg, report.Stats, report.Observed, report.Accepted)
	buf.WriteString(fmt.Sprintf("Clients report (ranking %s, top %s, displayed_groups %d, omitted_groups %d, omitted_slow_ops %d)\n", report.Ranking, topLabel(report.TopLimit), report.DisplayedGroups, report.OmittedGroups, report.OmittedSlowOps+report.UncorrelatedSlowOperations))
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}

	columns := visibleClientOperationColumns(report.Rows)
	for _, group := range groupClientRowsForTerminal(report.Rows) {
		buf.WriteString(group.user + "\n")
		bl := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
		fmt.Fprint(bl, "CLIENT_IP\tTOTAL")
		for _, col := range columns {
			fmt.Fprint(bl, "\t"+col)
		}
		fmt.Fprint(bl, "\n")
		for _, row := range group.rows {
			fmt.Fprintf(bl, "%s\t%d", row.ClientIP, row.LoggedSlowOperations)
			for _, col := range columns {
				fmt.Fprintf(bl, "\t%d", row.CountsByOperation[col])
			}
			fmt.Fprint(bl, "\n")
		}
		if err := bl.Flush(); err != nil {
			return "", 1, err
		}
	}
	return buf.String(), 0, nil
}

func captureClientMetadata(raw map[string]any, connections map[string]*clientConnectionInfo) {
	ctx := sanitizeBoundedString(asString(raw["ctx"]), 128)
	if ctx == "" {
		return
	}
	msg := strings.ToLower(asString(raw["msg"]))
	if msg != "connection accepted" && msg != "connection ended" && msg != "client metadata" && msg != "authentication succeeded" {
		return
	}
	attr, _ := raw["attr"].(map[string]any)
	if attr == nil {
		attr = map[string]any{}
	}
	conn := connections[ctx]
	if conn == nil {
		conn = &clientConnectionInfo{Ctx: ctx}
		connections[ctx] = conn
	}
	if id := sanitizeBoundedString(asString(attr["connectionId"]), 80); id != "" {
		conn.ConnectionID = id
	}
	if remote := sanitizeBoundedString(asString(attr["remote"]), 256); remote != "" {
		conn.Remote = remote
		conn.ClientIP = clientIPFromRemote(remote)
	}

	switch msg {
	case "client metadata":
		doc, _ := attr["doc"].(map[string]any)
		if value := metadataPath(doc, "application", "name"); value != "" {
			conn.ApplicationName = value
		}
		if value := metadataPath(doc, "driver", "name"); value != "" {
			conn.DriverName = value
		}
		if value := metadataPath(doc, "driver", "version"); value != "" {
			conn.DriverVersion = value
		}
		if value := metadataPath(doc, "os", "type"); value != "" {
			conn.OSType = value
		}
		if value := metadataPath(doc, "os", "name"); value != "" {
			conn.OSName = value
		}
		if value := metadataPath(doc, "os", "version"); value != "" {
			conn.OSVersion = value
		}
	case "authentication succeeded":
		if value := sanitizeClientValue(asString(attr["principalName"])); value != "" {
			conn.PrincipalName = value
		}
		if value := sanitizeClientValue(asString(attr["authenticationDatabase"])); value != "" {
			conn.AuthenticationDatabase = value
		}
		if value := sanitizeClientValue(asString(attr["mechanism"])); value != "" {
			conn.Mechanism = value
		}
	}
}

func (a *clientAgg) add(op *SlowOperation, conn *clientConnectionInfo, remote string) {
	a.Count++
	a.DurationMs += op.DurationMillis
	a.Hist.Add(op.Duration)
	operation := op.Operation
	if !containsString(clientOperationColumns, operation) {
		operation = "other"
	}
	a.Operations[operation]++
	a.Namespaces[op.Namespace]++
	if op.DocsExamined.Valid {
		a.DocsExamined += op.DocsExamined.Value
	}
	if conn.ApplicationName != "" {
		a.Applications[conn.ApplicationName] = struct{}{}
	}
	if conn.DriverName != "" || conn.DriverVersion != "" {
		driver := conn.DriverName
		if conn.DriverVersion != "" {
			if driver == "" {
				driver = conn.DriverVersion
			} else {
				driver += " " + conn.DriverVersion
			}
		}
		a.Drivers[driver] = struct{}{}
	}
	if remote != "" {
		a.Remotes[remote] = struct{}{}
	}
	if conn.ConnectionID != "" {
		a.ConnectionIDs[conn.ConnectionID] = struct{}{}
	}
	if len(a.Samples) < 5 {
		a.Samples[formatSampleLine(op.SourceFile, op.SourceLine)] = struct{}{}
	}
}

func clientAggRow(agg *clientAgg) clientsRowJSON {
	counts := map[string]int{}
	for _, col := range clientOperationColumns {
		counts[col] = agg.Operations[col]
	}
	return clientsRowJSON{
		User:                   agg.User,
		AuthenticationDatabase: agg.AuthenticationDatabase,
		ClientIP:               agg.ClientIP,
		Applications:           sortedSet(agg.Applications, 10),
		Drivers:                sortedSet(agg.Drivers, 10),
		Remotes:                sortedSet(agg.Remotes, 10),
		LoggedSlowOperations:   agg.Count,
		CumulativeDurationMs:   agg.DurationMs,
		LatencyMs:              agg.Hist.Summary(false),
		CountsByOperation:      counts,
		TopNamespaces:          topCounts(agg.Namespaces, 5),
		SampleLines:            sortedSet(agg.Samples, 5),
		terminalRank:           agg.BestRank,
	}
}

func selectClientAggsByRanking(aggs map[string]*clientAgg, top int, sortBy string) []string {
	keys := make([]string, 0, len(aggs))
	for key := range aggs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a := aggs[keys[i]]
		b := aggs[keys[j]]
		av := clientRankingValue(a, sortBy)
		bv := clientRankingValue(b, sortBy)
		if av != bv {
			return av > bv
		}
		if a.DurationMs != b.DurationMs {
			return a.DurationMs > b.DurationMs
		}
		if a.User != b.User {
			return a.User < b.User
		}
		if a.AuthenticationDatabase != b.AuthenticationDatabase {
			return a.AuthenticationDatabase < b.AuthenticationDatabase
		}
		return a.ClientIP < b.ClientIP
	})
	if top > 0 && len(keys) > top {
		keys = keys[:top]
	}
	return keys
}

func clientRankingValue(agg *clientAgg, sortBy string) int64 {
	summary := agg.Hist.Summary(false)
	switch sortBy {
	case "total-duration":
		return agg.DurationMs
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
		return agg.DocsExamined
	default:
		return int64(agg.Count)
	}
}

func omittedClientTotals(aggs map[string]*clientAgg, selected map[string]struct{}) (int, int, int64) {
	var groups int
	var ops int
	var ms int64
	for key, agg := range aggs {
		if _, ok := selected[key]; ok {
			continue
		}
		groups++
		ops += agg.Count
		ms += agg.DurationMs
	}
	return groups, ops, ms
}

func visibleClientOperationColumns(rows []clientsRowJSON) []string {
	var out []string
	for _, col := range clientOperationColumns {
		for _, row := range rows {
			if row.CountsByOperation[col] > 0 {
				out = append(out, col)
				break
			}
		}
	}
	return out
}

type terminalClientGroup struct {
	user string
	rank int
	rows []clientsRowJSON
}

func groupClientRowsForTerminal(rows []clientsRowJSON) []terminalClientGroup {
	groupsByUser := map[string]*terminalClientGroup{}
	for _, row := range rows {
		user := clientUserLabel(row.User, row.AuthenticationDatabase)
		group := groupsByUser[user]
		if group == nil {
			group = &terminalClientGroup{user: user, rank: row.terminalRank}
			groupsByUser[user] = group
		}
		if row.terminalRank < group.rank {
			group.rank = row.terminalRank
		}
		group.rows = append(group.rows, row)
	}
	groups := make([]terminalClientGroup, 0, len(groupsByUser))
	for _, group := range groupsByUser {
		sort.Slice(group.rows, func(i, j int) bool {
			return group.rows[i].terminalRank < group.rows[j].terminalRank
		})
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].rank != groups[j].rank {
			return groups[i].rank < groups[j].rank
		}
		return groups[i].user < groups[j].user
	})
	return groups
}

func clientUserLabel(user, authDB string) string {
	if user == "" {
		user = "<unknown>"
	}
	if authDB == "" {
		authDB = "<unknown>"
	}
	return user + "@" + authDB
}

func clientIPFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "[") {
		end := strings.Index(remote, "]")
		if end > 1 {
			return remote[1:end]
		}
	}
	if idx := strings.LastIndex(remote, ":"); idx > 0 && strings.Count(remote, ":") == 1 {
		return remote[:idx]
	}
	return remote
}

func metadataPath(root map[string]any, keys ...string) string {
	var current any = root
	for _, key := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = obj[key]
	}
	return sanitizeClientValue(asString(current))
}

func sanitizeClientValue(value string) string {
	return sanitizeBoundedString(value, 160)
}

func sortedSet(values map[string]struct{}, limit int) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func clientsSortBy(cfg Config) string {
	if cfg.SortBy == "" {
		return "count"
	}
	return cfg.SortBy
}
