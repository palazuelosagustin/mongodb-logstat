package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"
)

// Node identity. Each input file is attributed to the mongod or mongos that
// wrote it, from lines MongoDB writes at startup and again at every log
// rotation (6.0+), so rotated files of one process merge into one node:
//
//	4615611 MongoDB starting          host, port (mongod)
//	20721   Process Details           host, port (mongod and mongos, at rotation)
//	20722   Node is a member of a replica set   config._id
//	21392   New replica set config in use       config._id
//	21951   Options set by command line         net.port, replication.replSetName,
//	                                            sharding.clusterRole, sharding.configDB (mongos)
//	23403   Build Info                version
//
// A file with none of these is its own node, named by its file label.

type fileInfo struct {
	Label       string
	Host        string
	Port        string
	Process     string
	ReplSet     string
	ClusterRole string
	Version     string
	From        *time.Time
	To          *time.Time
	SlowOps     int
}

// NodeSummary is one mongod or mongos and the files attributed to it.
type NodeSummary struct {
	Node           string     `json:"node"`
	Host           string     `json:"host,omitempty"`
	Port           string     `json:"port,omitempty"`
	Process        string     `json:"process,omitempty"`
	ReplSet        string     `json:"replSet,omitempty"`
	ClusterRole    string     `json:"clusterRole,omitempty"`
	Version        string     `json:"version,omitempty"`
	Files          []string   `json:"files"`
	From           *time.Time `json:"-"`
	To             *time.Time `json:"-"`
	SlowOperations int        `json:"slowOperations"`
}

func (n NodeSummary) MarshalJSON() ([]byte, error) {
	type alias NodeSummary
	return json.Marshal(struct {
		alias
		From *string `json:"from"`
		To   *string `json:"to"`
	}{alias(n), formatJSONTimePtr(n.From), formatJSONTimePtr(n.To)})
}

var identityLogIDs = map[int64]bool{4615611: true, 20721: true, 20722: true, 21392: true, 21951: true, 23403: true}

func (s *PassStats) fileInfo(label string) *fileInfo {
	if s.Files == nil {
		s.Files = map[string]*fileInfo{}
	}
	info := s.Files[label]
	if info == nil {
		info = &fileInfo{Label: label}
		s.Files[label] = info
	}
	return info
}

func scalarText(v any) string {
	switch v.(type) {
	case nil, map[string]any, []any:
		return ""
	}
	return sanitizeBoundedString(fmt.Sprint(v), 200)
}

func mapAt(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		next, _ := m[key].(map[string]any)
		if next == nil {
			return nil
		}
		m = next
	}
	return m
}

// observeIdentity records node identity facts from one structured line.
func observeIdentity(info *fileInfo, id int64, raw map[string]any) {
	attr, _ := raw["attr"].(map[string]any)
	if attr == nil {
		return
	}
	switch id {
	case 4615611:
		info.Process = "mongod"
		fallthrough
	case 20721:
		if host := scalarText(attr["host"]); host != "" {
			info.Host = host
		}
		if port := scalarText(attr["port"]); port != "" {
			info.Port = port
		}
	case 20722, 21392:
		if set := scalarText(mapAt(attr, "config")["_id"]); set != "" {
			info.ReplSet = set
		}
	case 21951:
		options := mapAt(attr, "options")
		if port := scalarText(mapAt(options, "net")["port"]); port != "" && info.Port == "" {
			info.Port = port
		}
		if set := scalarText(mapAt(options, "replication")["replSetName"]); set != "" && info.ReplSet == "" {
			info.ReplSet = set
		}
		sharding := mapAt(options, "sharding")
		if role := scalarText(sharding["clusterRole"]); role != "" {
			info.ClusterRole = role
		}
		if scalarText(sharding["configDB"]) != "" {
			info.Process = "mongos"
		} else if info.Process == "" {
			info.Process = "mongod"
		}
	case 23403:
		if version := scalarText(mapAt(attr, "buildInfo")["version"]); version != "" {
			info.Version = version
		}
		if asString(raw["ctx"]) == "mongosMain" {
			info.Process = "mongos"
		}
	}
}

func (info *fileInfo) nodeName() string {
	switch {
	case info.Host != "" && info.Port != "":
		return info.Host + ":" + info.Port
	case info.Host != "":
		return info.Host
	default:
		return info.Label
	}
}

// buildNodes groups files into nodes and maps each file label to its node.
// A file that only repeated an earlier copy (no lines of its own were read)
// belongs to the node of the file it repeats.
func buildNodes(stats *PassStats) {
	stats.NodeOf = map[string]string{}
	if len(stats.Files) == 0 {
		return
	}
	labels := make([]string, 0, len(stats.Files))
	for label := range stats.Files {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	copyOf := map[string]string{}
	for _, d := range stats.Duplicates {
		copyOf[d.Path] = d.Of
	}

	nodes := map[string]*NodeSummary{}
	for _, label := range labels {
		info := stats.Files[label]
		if info.Host == "" && info.Process == "" && info.Version == "" {
			if of, ok := copyOf[label]; ok && stats.Files[of] != nil {
				info = mergedIdentity(info, stats.Files[of])
			}
		}
		name := info.nodeName()
		stats.NodeOf[label] = name
		node := nodes[name]
		if node == nil {
			node = &NodeSummary{Node: name}
			nodes[name] = node
		}
		fill := func(dst *string, v string) {
			if *dst == "" {
				*dst = v
			}
		}
		fill(&node.Host, info.Host)
		fill(&node.Port, info.Port)
		fill(&node.Process, info.Process)
		fill(&node.ReplSet, info.ReplSet)
		fill(&node.ClusterRole, info.ClusterRole)
		if info.Version != "" {
			node.Version = info.Version // the latest file read wins (upgrades)
		}
		node.Files = append(node.Files, label)
		node.SlowOperations += info.SlowOps
		if info.From != nil {
			updateObservedRange(&node.From, &node.To, *info.From)
		}
		if info.To != nil {
			updateObservedRange(&node.From, &node.To, *info.To)
		}
	}
	stats.Nodes = make([]NodeSummary, 0, len(nodes))
	for _, node := range nodes {
		stats.Nodes = append(stats.Nodes, *node)
	}
	sort.Slice(stats.Nodes, func(i, j int) bool { return stats.Nodes[i].Node < stats.Nodes[j].Node })
}

// mergedIdentity gives a copy file the identity of the file it repeats.
func mergedIdentity(copyInfo, original *fileInfo) *fileInfo {
	out := *original
	out.Label = copyInfo.Label
	out.From, out.To, out.SlowOps = copyInfo.From, copyInfo.To, copyInfo.SlowOps
	return &out
}

// nodeOf returns the node for a file label, or the label itself.
func (s *PassStats) nodeOf(label string) string {
	if node, ok := s.NodeOf[label]; ok {
		return node
	}
	return label
}

// writeNodesTable prints the nodes in the terminal header. It is left out
// when no file had any identity line (nothing to add to "Files:").
func writeNodesTable(buf *bytes.Buffer, stats PassStats) {
	identified := false
	for _, n := range stats.Nodes {
		if n.Host != "" || n.Process != "" || n.Version != "" {
			identified = true
			break
		}
	}
	if !identified {
		return
	}
	buf.WriteString(fmt.Sprintf("Nodes:                %d\n", len(stats.Nodes)))
	tw := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "  node\tprocess\treplSet\trole\tversion\tfiles\tlog range\tslow ops\n")
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, n := range stats.Nodes {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%d\t%s to %s\t%d\n", n.Node, dash(n.Process), dash(n.ReplSet), dash(n.ClusterRole), dash(n.Version), len(n.Files), formatMaybeTime(n.From), formatMaybeTime(n.To), n.SlowOperations)
	}
	tw.Flush()
}

func countLabel(counts map[string]int, unit string) string {
	if len(counts) == 1 {
		for name := range counts {
			return name
		}
	}
	return fmt.Sprintf("%d %s", len(counts), unit)
}
