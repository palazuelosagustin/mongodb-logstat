package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

var errUsage = errors.New("usage")

type Config struct {
	Report  string
	GroupBy string
	Bucket  time.Duration
	Top     int
	JSON    bool
	Verbose bool
	From    *time.Time
	To      *time.Time
	Inputs  []string
}

func parseCLI(args []string) (Config, error) {
	fs := flag.NewFlagSet("mongodb-logstat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	cfg := Config{}
	from := fs.String("from", "", "RFC3339 lower time bound")
	to := fs.String("to", "", "RFC3339 upper time bound")
	bucket := fs.String("bucket", "1h", "timeline bucket size")
	fs.StringVar(&cfg.Report, "report", "summary", "summary|timeline|queries")
	fs.StringVar(&cfg.GroupBy, "group-by", "optype", "timeline grouping: optype|namespace|query")
	fs.IntVar(&cfg.Top, "top", 0, "limit rows or columns to the top N; 0 prints all")
	fs.BoolVar(&cfg.JSON, "json", false, "emit JSON")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "print per-file failures to stderr")

	normalized := normalizeCLIArgs(args)
	if err := fs.Parse(normalized); err != nil {
		return Config{}, errUsage
	}

	var err error
	cfg.Bucket, err = time.ParseDuration(*bucket)
	if err != nil || cfg.Bucket <= 0 {
		return Config{}, fmt.Errorf("invalid --bucket: %q", *bucket)
	}
	if cfg.Top < 0 {
		return Config{}, fmt.Errorf("invalid --top: %d", cfg.Top)
	}

	cfg.Report = strings.TrimSpace(cfg.Report)
	switch cfg.Report {
	case "summary", "timeline", "queries":
	default:
		return Config{}, fmt.Errorf("invalid --report: %q", cfg.Report)
	}

	cfg.GroupBy = strings.TrimSpace(cfg.GroupBy)
	switch cfg.GroupBy {
	case "optype", "namespace", "query":
	default:
		return Config{}, fmt.Errorf("invalid --group-by: %q", cfg.GroupBy)
	}

	if *from != "" {
		parsed, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return Config{}, fmt.Errorf("invalid --from: %w", err)
		}
		cfg.From = &parsed
	}
	if *to != "" {
		parsed, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return Config{}, fmt.Errorf("invalid --to: %w", err)
		}
		cfg.To = &parsed
	}
	if cfg.From != nil && cfg.To != nil && cfg.From.After(*cfg.To) {
		return Config{}, fmt.Errorf("--from must be <= --to")
	}

	cfg.Inputs = fs.Args()
	if len(cfg.Inputs) == 0 {
		return Config{}, errUsage
	}

	return cfg, nil
}

func normalizeCLIArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}

		flags = append(flags, arg)
		if flagTakesValue(arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positionals...)
}

func flagTakesValue(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if idx := strings.IndexByte(name, '='); idx >= 0 {
		name = name[:idx]
	}
	switch name {
	case "from", "to", "bucket", "report", "group-by", "top":
		return true
	default:
		return false
	}
}

func usageText() string {
	return "usage: mongodb-logstat [--report summary|timeline|queries] [--json] [--from RFC3339] [--to RFC3339] [--bucket 1h] [--group-by optype|namespace|query] [--top N] [--verbose] <file-or-dir> [...]"
}
