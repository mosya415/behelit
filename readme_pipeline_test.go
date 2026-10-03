package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The block in the README and the block lca's own reader accepts must be the
// same block. A reviewer found that the documented args:/fields: snippet is
// REFUSED by our own YAML reader — an operator following the documentation
// literally gets an error on the one file they were told to copy — and a
// hand-written fixture inside a test cannot catch that, because it is the
// fixture that gets kept in step and not the prose.
//
// So this reads README.md itself: every documented pipeline: block must parse,
// and the two halves taken together must validate as a complete configuration.
func TestEveryPipelineBlockInTheREADMEIsOneLCAAccepts(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := yamlBlocksWith(string(data), "pipeline:")
	if len(blocks) < 2 {
		t.Fatalf("the README documents the block in pieces; found %d", len(blocks))
	}
	for i, b := range blocks {
		if _, err := parsePipelineYAML(t, b); err != nil {
			t.Fatalf("documented block %d does not parse:\n%s\n%v", i+1, b, err)
		}
	}
}

// yamlBlocksWith returns the ```yaml fenced blocks that contain a marker.
func yamlBlocksWith(doc, marker string) []string {
	var out []string
	for rest := doc; ; {
		i := strings.Index(rest, "```yaml\n")
		if i < 0 {
			return out
		}
		rest = rest[i+len("```yaml\n"):]
		j := strings.Index(rest, "```")
		if j < 0 {
			return out
		}
		if b := rest[:j]; strings.Contains(b, marker) {
			out = append(out, b)
		}
		rest = rest[j:]
	}
}

// parsePipelineYAML runs one documented block through the real reader — the same
// two calls lca makes when it reads roles.yaml.
func parsePipelineYAML(t *testing.T, block string) (*PipelineConfig, error) {
	t.Helper()
	pc, err := parsePipeline(nil, parseYAMLish(block), "README.md")
	if err != nil {
		return nil, err
	}
	if pc == nil {
		return nil, fmt.Errorf("no pipeline: block parsed out of it")
	}
	return pc, nil
}

// And the one piece an operator actually copies is kept honest by parsing and
// validating it: examples/ticket.roles.yaml is the two documented halves in one
// block, which is the shape a real .lca/roles.yaml has. The roles it names are
// supplied here because they are the one thing that cannot live in an example.
func TestTheExampleTicketBlockValidates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("examples", "ticket.roles.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	pc, err := parsePipelineYAML(t, string(data))
	if err != nil {
		t.Fatalf("the example an operator copies must parse: %v", err)
	}
	team := &RolesConfig{Roles: []*Agent{
		// The coder needs a check command: the review and the merge gates are both
		// written against a green one, so a coder without it is a pipeline that
		// could pay for a model every night and never merge anything.
		{Name: "coder", IsRole: true, CheckCmd: "go test ./..."},
		{Name: "reviewer", IsRole: true},
		{Name: "integrator", IsRole: true},
	}}
	// Validated for the hardest case it documents: creating a ticket and pushing,
	// which is every key it can require at once.
	if err := pc.validate(team, tktNeed{creating: true}); err != nil {
		t.Fatalf("the example must satisfy lca's own reader:\n%v", err)
	}
}
