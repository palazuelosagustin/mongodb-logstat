package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

// The README is the reference for humans and agents; these checks keep it
// in step with the CLI.

func readREADME(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestREADMEUsageMatchesCLI(t *testing.T) {
	readme := readREADME(t)
	usage := strings.TrimPrefix(usageText(), "usage: ")
	if !strings.Contains(readme, usage) {
		t.Fatalf("README usage block does not match usageText():\n%s", usage)
	}
}

func TestREADMEDocumentsEveryReport(t *testing.T) {
	readme := readREADME(t)
	for _, report := range []string{"timeline", "queries", "clients", "analysis", "events", "messages", "connections"} {
		if !strings.Contains(readme, "### `"+report+"`") {
			t.Errorf("README has no section for report %q", report)
		}
		if _, err := parseCLI([]string{"--report", report, "x.log"}); err != nil {
			t.Errorf("report %q in the README list is rejected by the CLI: %v", report, err)
		}
	}
}

func TestREADMEMentionsEveryFlag(t *testing.T) {
	readme := readREADME(t)
	var cfg Config
	fs, _, _, _ := newCLIFlagSet(&cfg)
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(readme, "--"+f.Name) {
			t.Errorf("README does not mention --%s", f.Name)
		}
	})
}

func TestREADMEDocumentsEventKinds(t *testing.T) {
	readme := readREADME(t)
	kinds := map[string]bool{"fatal": true}
	for _, spec := range eventSpecs {
		kinds[spec.kind] = true
	}
	for kind := range kinds {
		if !strings.Contains(readme, "`"+kind+"`") {
			t.Errorf("README events table does not mention kind %q", kind)
		}
	}
}

// The Claude and Codex skills carry the same instructions and references;
// only the frontmatter name differs.
func TestSkillsAreAligned(t *testing.T) {
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	body := func(skill string) string {
		parts := strings.SplitN(skill, "---", 3)
		if len(parts) != 3 {
			t.Fatalf("skill without frontmatter")
		}
		return parts[2]
	}
	frontmatterLine := func(skill, key string) string {
		for _, line := range strings.Split(strings.SplitN(skill, "---", 3)[1], "\n") {
			if strings.HasPrefix(line, key+":") {
				return line
			}
		}
		return ""
	}
	claude := read("skills/.claude/mongodb-logstat/SKILL.md")
	codex := read(".codex/skills/mongodb-logstat-skill/SKILL.md")
	if body(claude) != body(codex) {
		t.Errorf("Claude and Codex SKILL.md bodies differ")
	}
	if frontmatterLine(claude, "description") != frontmatterLine(codex, "description") {
		t.Errorf("Claude and Codex skill descriptions differ")
	}
	if read("skills/.claude/mongodb-logstat/references/json.md") != read(".codex/skills/mongodb-logstat-skill/references/json.md") {
		t.Errorf("references/json.md differs between the Claude and Codex skills")
	}
	usage := strings.TrimPrefix(usageText(), "usage: ")
	if !strings.Contains(claude, usage) {
		t.Errorf("skill usage block does not match usageText()")
	}
}
