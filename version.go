package main

import (
	"fmt"
	"runtime/debug"
)

// Release builds set these with -ldflags -X main.buildVersion=...; a plain
// `go build` reports "dev" plus the VCS revision Go embeds in the binary.
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildDate    = ""
)

// toolVersion is the version printed by --version and written to JSON, for
// example "v0.1.0" or "dev (3f2c1ab, modified)".
func toolVersion() string {
	if buildVersion != "dev" {
		return buildVersion
	}
	commit, modified := buildCommit, false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = s.Value
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	commit = shortCommit(commit)
	switch {
	case commit == "":
		return "dev"
	case modified:
		return fmt.Sprintf("dev (%s, modified)", commit)
	default:
		return fmt.Sprintf("dev (%s)", commit)
	}
}

// versionLine is the --version output.
func versionLine() string {
	s := "mongodb-logstat " + toolVersion()
	if buildVersion != "dev" && buildCommit != "" {
		s += " (" + shortCommit(buildCommit)
		if buildDate != "" {
			s += ", " + buildDate
		}
		s += ")"
	}
	return s
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// isVersionRequest reports a lone --version / -version argument.
func isVersionRequest(args []string) bool {
	return len(args) == 1 && (args[0] == "--version" || args[0] == "-version")
}
