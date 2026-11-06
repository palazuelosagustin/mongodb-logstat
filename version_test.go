package main

import (
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	defer func(v, c, d string) { buildVersion, buildCommit, buildDate = v, c, d }(buildVersion, buildCommit, buildDate)

	buildVersion, buildCommit, buildDate = "v0.1.0", "0123456789abcdef", "2026-11-06T12:00:00Z"
	if got, want := versionLine(), "mongodb-logstat v0.1.0 (0123456, 2026-11-06T12:00:00Z)"; got != want {
		t.Fatalf("versionLine() = %q, want %q", got, want)
	}
	if got := toolVersion(); got != "v0.1.0" {
		t.Fatalf("toolVersion() = %q", got)
	}

	buildVersion, buildCommit, buildDate = "dev", "", ""
	if got := toolVersion(); !strings.HasPrefix(got, "dev") {
		t.Fatalf("source build toolVersion() = %q, want dev...", got)
	}
}

func TestVersionRequest(t *testing.T) {
	if !isVersionRequest([]string{"--version"}) || !isVersionRequest([]string{"-version"}) {
		t.Fatal("--version not recognised")
	}
	if isVersionRequest([]string{"--version", "x.log"}) || isVersionRequest(nil) {
		t.Fatal("--version must be the only argument")
	}
}
