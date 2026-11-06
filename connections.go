package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// The connections report counts connection and authentication events per
// time bucket and node, lists the client IPs that opened connections, and
// groups authentication failures. Facts only: no thresholds.
//
// Log ids (6.0-8.2):
//
//	22943   Connection accepted         remote, connectionId, connectionCount
//	22944   Connection ended            remote, connectionCount
//	22942   Connection refused because there are too many open connections
//	51800   client metadata             remote, doc.application.name, doc.driver
//	20250   Authentication succeeded    6.0: principalName, authenticationDatabase, remote
//	5286306 Successfully authenticated  7.0+: user, db, client
//	20249   Authentication failed       6.0: principalName, authenticationDatabase, error
//	5286307 Failed to authenticate      7.0+: user, db, mechanism, client, error

const (
	connAccepted = iota
	connEnded
	connRefused
	connMetadata
	connAuthOK
	connAuthFailed
)

var connectionEventIDs = map[int64]int{
	22943: connAccepted, 22944: connEnded, 22942: connRefused, 51800: connMetadata,
	20250: connAuthOK, 5286306: connAuthOK, 20249: connAuthFailed, 5286307: connAuthFailed,
}

type ConnectionRow struct {
	Start         time.Time `json:"-"`
	Node          string    `json:"node"`
	Accepted      int       `json:"accepted"`
	Ended         int       `json:"ended"`
	OpenMax       *int64    `json:"openMax"`
	OpenLast      *int64    `json:"openLast"`
	AuthSucceeded int       `json:"authSucceeded"`
	AuthFailed    int       `json:"authFailed"`
	Refused       int       `json:"refused"`
	lastSeen      time.Time
}

type connectionRowJSON struct {
	Start string `json:"start"`
	ConnectionRow
}

type ConnectionClient struct {
	ClientIP     string   `json:"clientIP"`
	Accepted     int      `json:"accepted"`
	Ended        int      `json:"ended"`
	AuthFailed   int      `json:"authFailed"`
	Refused      int      `json:"refused"`
	Applications []string `json:"applications"`
	Drivers      []string `json:"drivers"`
	Nodes        []string `json:"nodes"`
}

type AuthFailure struct {
	User      string   `json:"user"`
	DB        string   `json:"db"`
	Mechanism string   `json:"mechanism"`
	Error     string   `json:"error"`
	Count     int      `json:"count"`
	First     string   `json:"first"`
	Last      string   `json:"last"`
	ClientIPs []string `json:"clientIPs"`
	Nodes     []string `json:"nodes"`
}

type ConnectionsReport struct {
	Observed       Range
	Stats          PassStats
	Accepted       int
	Rows           []ConnectionRow
	Totals         ConnectionRow
	Clients        []ConnectionClient
	OmittedClients int
	AuthFailures   []AuthFailure
	OmittedFailure int
}

type connectionsReportJSON struct {
	reportEnvelope
	Bucket              string              `json:"bucket"`
	Totals              ConnectionRow       `json:"totals"`
	Rows                []connectionRowJSON `json:"rows"`
	Clients             []ConnectionClient  `json:"clients"`
	OmittedClients      int                 `json:"omittedClients"`
	AuthFailures        []AuthFailure       `json:"authFailures"`
	OmittedAuthFailures int                 `json:"omittedAuthFailures"`
}

type connClientAgg struct {
	ip                               string
	accepted, ended, failed, refused int
	apps, drivers, files             map[string]struct{}
}

type authFailAgg struct {
	key         [4]string
	count       int
	first, last time.Time
	ips, files  map[string]struct{}
}

func setOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// authErrorName keeps the error code name ("MechanismUnavailable") and
// drops the free text after it, which can carry user or host details.
func authErrorName(v any) string {
	switch e := v.(type) {
	case map[string]any:
		if name := asString(e["codeName"]); name != "" {
			return sanitizeBoundedString(name, 80)
		}
		return sanitizeBoundedString(asString(e["errmsg"]), 120)
	default:
		s := asString(v)
		if i := strings.Index(s, ":"); i > 0 {
			s = s[:i]
		}
		return sanitizeBoundedString(s, 80)
	}
}

func computeConnections(files []string, discoveryFailures []FileFailure, cfg Config) ConnectionsReport {
	report := ConnectionsReport{Stats: newPassStats(files, discoveryFailures)}
	type rowKey struct {
		bucket int64
		file   string
	}
	rows := map[rowKey]*ConnectionRow{}
	clients := map[string]*connClientAgg{}
	failures := map[[4]string]*authFailAgg{}

	client := func(ip string) *connClientAgg {
		c := clients[ip]
		if c == nil {
			c = &connClientAgg{ip: ip, apps: map[string]struct{}{}, drivers: map[string]struct{}{}, files: map[string]struct{}{}}
			clients[ip] = c
		}
		return c
	}

	parseFiles(files, parseOptions{
		From: cfg.From,
		To:   cfg.To,
		OnStructuredEvent: func(event StructuredEvent) {
			id, _ := asInt64(event.Raw["id"])
			kind, ok := connectionEventIDs[id]
			if !ok {
				return
			}
			attr, _ := event.Raw["attr"].(map[string]any)
			if attr == nil {
				attr = map[string]any{}
			}
			ip := clientIPFromRemote(sanitizeBoundedString(firstString(attr, "remote", "client"), 256))
			bucket := event.Timestamp.UTC().Truncate(cfg.Bucket)
			key := rowKey{bucket.UnixNano(), event.SourceFile}
			row := rows[key]
			if row == nil {
				row = &ConnectionRow{Start: bucket}
				rows[key] = row
			}
			if count, ok := asInt64(attr["connectionCount"]); ok && (kind == connAccepted || kind == connEnded) {
				if row.OpenMax == nil || count > *row.OpenMax {
					v := count
					row.OpenMax = &v
				}
				if row.OpenLast == nil || !event.Timestamp.Before(row.lastSeen) {
					v := count
					row.OpenLast = &v
					row.lastSeen = event.Timestamp
				}
			}
			switch kind {
			case connAccepted:
				row.Accepted++
				if ip != "" {
					c := client(ip)
					c.accepted++
					c.files[event.SourceFile] = struct{}{}
				}
			case connEnded:
				row.Ended++
				if ip != "" {
					client(ip).ended++
				}
			case connRefused:
				row.Refused++
				if ip != "" {
					c := client(ip)
					c.refused++
					c.files[event.SourceFile] = struct{}{}
				}
			case connMetadata:
				if ip == "" {
					return
				}
				doc, _ := attr["doc"].(map[string]any)
				c := client(ip)
				if app := metadataPath(doc, "application", "name"); app != "" {
					c.apps[app] = struct{}{}
				}
				if driver := strings.TrimSpace(metadataPath(doc, "driver", "name") + " " + metadataPath(doc, "driver", "version")); driver != "" {
					c.drivers[driver] = struct{}{}
				}
			case connAuthOK:
				row.AuthSucceeded++
			case connAuthFailed:
				row.AuthFailed++
				if ip != "" {
					c := client(ip)
					c.failed++
					c.files[event.SourceFile] = struct{}{}
				}
				fk := [4]string{
					sanitizeClientValue(firstString(attr, "user", "principalName")),
					sanitizeClientValue(firstString(attr, "db", "authenticationDatabase")),
					sanitizeClientValue(asString(attr["mechanism"])),
					authErrorName(attr["error"]),
				}
				f := failures[fk]
				if f == nil {
					f = &authFailAgg{key: fk, first: event.Timestamp, last: event.Timestamp, ips: map[string]struct{}{}, files: map[string]struct{}{}}
					failures[fk] = f
				}
				f.count++
				if event.Timestamp.Before(f.first) {
					f.first = event.Timestamp
				}
				if event.Timestamp.After(f.last) {
					f.last = event.Timestamp
				}
				if ip != "" {
					f.ips[ip] = struct{}{}
				}
				f.files[event.SourceFile] = struct{}{}
			}
		},
	}, func(op *SlowOperation) {
		report.Accepted++
		updateObservedRange(&report.Observed.From, &report.Observed.To, op.Timestamp)
	}, &report.Stats)
	finalizeStats(&report.Stats)

	// Merge per-file rows into per-node rows.
	type nodeKey struct {
		bucket int64
		node   string
	}
	merged := map[nodeKey]*ConnectionRow{}
	for key, row := range rows {
		node := report.Stats.nodeOf(key.file)
		nk := nodeKey{key.bucket, node}
		m := merged[nk]
		if m == nil {
			m = &ConnectionRow{Start: row.Start, Node: node}
			merged[nk] = m
		}
		m.Accepted += row.Accepted
		m.Ended += row.Ended
		m.Refused += row.Refused
		m.AuthSucceeded += row.AuthSucceeded
		m.AuthFailed += row.AuthFailed
		if row.OpenMax != nil && (m.OpenMax == nil || *row.OpenMax > *m.OpenMax) {
			m.OpenMax = row.OpenMax
		}
		if row.OpenLast != nil && (m.OpenLast == nil || row.lastSeen.After(m.lastSeen)) {
			m.OpenLast, m.lastSeen = row.OpenLast, row.lastSeen
		}
	}
	for _, row := range merged {
		report.Rows = append(report.Rows, *row)
		report.Totals.Accepted += row.Accepted
		report.Totals.Ended += row.Ended
		report.Totals.Refused += row.Refused
		report.Totals.AuthSucceeded += row.AuthSucceeded
		report.Totals.AuthFailed += row.AuthFailed
		if row.OpenMax != nil && (report.Totals.OpenMax == nil || *row.OpenMax > *report.Totals.OpenMax) {
			report.Totals.OpenMax = row.OpenMax
		}
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		if !report.Rows[i].Start.Equal(report.Rows[j].Start) {
			return report.Rows[i].Start.Before(report.Rows[j].Start)
		}
		return report.Rows[i].Node < report.Rows[j].Node
	})

	top := effectiveTop(cfg, "connections")
	nodesOf := func(files map[string]struct{}) []string {
		set := map[string]struct{}{}
		for f := range files {
			set[report.Stats.nodeOf(f)] = struct{}{}
		}
		return setOf(set)
	}
	clientList := make([]*connClientAgg, 0, len(clients))
	for _, c := range clients {
		if c.accepted > 0 || c.failed > 0 || c.refused > 0 {
			clientList = append(clientList, c)
		}
	}
	sort.Slice(clientList, func(i, j int) bool {
		if clientList[i].accepted != clientList[j].accepted {
			return clientList[i].accepted > clientList[j].accepted
		}
		return clientList[i].ip < clientList[j].ip
	})
	for i, c := range clientList {
		if top > 0 && i >= top {
			report.OmittedClients++
			continue
		}
		report.Clients = append(report.Clients, ConnectionClient{ClientIP: c.ip, Accepted: c.accepted, Ended: c.ended, AuthFailed: c.failed, Refused: c.refused, Applications: setOf(c.apps), Drivers: setOf(c.drivers), Nodes: nodesOf(c.files)})
	}

	failList := make([]*authFailAgg, 0, len(failures))
	for _, f := range failures {
		failList = append(failList, f)
	}
	sort.Slice(failList, func(i, j int) bool {
		if failList[i].count != failList[j].count {
			return failList[i].count > failList[j].count
		}
		return strings.Join(failList[i].key[:], "\x00") < strings.Join(failList[j].key[:], "\x00")
	})
	for i, f := range failList {
		if top > 0 && i >= top {
			report.OmittedFailure++
			continue
		}
		report.AuthFailures = append(report.AuthFailures, AuthFailure{User: f.key[0], DB: f.key[1], Mechanism: f.key[2], Error: f.key[3], Count: f.count, First: formatEventTime(f.first), Last: formatEventTime(f.last), ClientIPs: setOf(f.ips), Nodes: nodesOf(f.files)})
	}
	return report
}

func renderConnections(cfg Config, report ConnectionsReport) (string, int, error) {
	if cfg.JSON {
		body := connectionsReportJSON{
			reportEnvelope:      newEnvelope("connections", cfg, report.Observed, report.Stats),
			Bucket:              cfg.Bucket.String(),
			Totals:              report.Totals,
			Rows:                make([]connectionRowJSON, 0, len(report.Rows)),
			Clients:             report.Clients,
			OmittedClients:      report.OmittedClients,
			AuthFailures:        report.AuthFailures,
			OmittedAuthFailures: report.OmittedFailure,
		}
		for _, row := range report.Rows {
			body.Rows = append(body.Rows, connectionRowJSON{Start: formatJSONTime(row.Start), ConnectionRow: row})
		}
		if body.Clients == nil {
			body.Clients = []ConnectionClient{}
		}
		if body.AuthFailures == nil {
			body.AuthFailures = []AuthFailure{}
		}
		return marshalJSON(body)
	}

	var buf bytes.Buffer
	writeInputStatus(&buf, cfg, report.Stats)
	writeTerminalMetadata(&buf, cfg, report.Stats, report.Observed, report.Accepted)
	t := report.Totals
	buf.WriteString(fmt.Sprintf("Connections report (bucket %s, gaps skipped): %d accepted, %d ended, open max %s, %d auth succeeded, %d auth failed, %d refused\n",
		cfg.Bucket, t.Accepted, t.Ended, optInt(t.OpenMax), t.AuthSucceeded, t.AuthFailed, t.Refused))
	if len(report.Rows) == 0 {
		buf.WriteString("No matching records.\n")
		return buf.String(), 0, nil
	}
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "start\tnode\taccepted\tended\topen_max\topen_last\tauth_ok\tauth_fail\trefused\n")
	for _, row := range report.Rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%d\t%d\t%d\n", row.Start.Format(time.RFC3339), row.Node, row.Accepted, row.Ended, optInt(row.OpenMax), optInt(row.OpenLast), row.AuthSucceeded, row.AuthFailed, row.Refused)
	}
	if err := tw.Flush(); err != nil {
		return "", 1, err
	}

	buf.WriteString(fmt.Sprintf("\nClients by connections accepted (top %s, omitted %d)\n", topLabel(effectiveTop(cfg, "connections")), report.OmittedClients))
	tw = tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "client_ip\taccepted\tended\tauth_fail\trefused\tnodes\tapplications\tdrivers\n")
	for _, c := range report.Clients {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", c.ClientIP, c.Accepted, c.Ended, c.AuthFailed, c.Refused, listLabel(c.Nodes, "nodes"), truncateRunes(joinOrDash(c.Applications), 60), truncateRunes(joinOrDash(c.Drivers), 50))
	}
	if err := tw.Flush(); err != nil {
		return "", 1, err
	}

	if len(report.AuthFailures) > 0 {
		buf.WriteString(fmt.Sprintf("\nAuthentication failures (top %s, omitted %d)\n", topLabel(effectiveTop(cfg, "connections")), report.OmittedFailure))
		tw = tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
		fmt.Fprint(tw, "user@db\tmechanism\terror\tcount\tfirst\tlast\tclient_ips\tnodes\n")
		for _, f := range report.AuthFailures {
			fmt.Fprintf(tw, "%s@%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", dashIfEmpty(f.User), dashIfEmpty(f.DB), dashIfEmpty(f.Mechanism), dashIfEmpty(f.Error), f.Count, f.First, f.Last, listLabel(f.ClientIPs, "IPs"), listLabel(f.Nodes, "nodes"))
		}
		if err := tw.Flush(); err != nil {
			return "", 1, err
		}
	}
	return buf.String(), 0, nil
}

func optInt(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

// listLabel is the single value, or "N <unit>" when there are several.
func listLabel(values []string, unit string) string {
	switch len(values) {
	case 0:
		return "-"
	case 1:
		return values[0]
	default:
		return fmt.Sprintf("%d %s", len(values), unit)
	}
}
