package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every finding ullage prints carries a Docs URL, and a reader who follows it
// has already decided to trust us enough to click. Shipping a check whose page
// does not exist spends that trust on a 404, so the registry and the docs/
// directory are checked against each other rather than kept in sync by hand.
func TestEveryCheckHasADocsPage(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "checks")

	for _, c := range All() {
		d := c.Describe()
		t.Run(d.ID, func(t *testing.T) {
			if d.Docs != docsURLFor(d.ID) {
				t.Fatalf("Docs = %q, want %q — build it with docsURLFor so the page is checked",
					d.Docs, docsURLFor(d.ID))
			}

			page := filepath.Join(dir, d.ID+".md")
			body, err := os.ReadFile(page)
			if err != nil {
				t.Fatalf("check %q publishes %s but %s does not exist: %v\n"+
					"every check needs a page describing what it claims and when it is wrong",
					d.ID, d.Docs, page, err)
			}

			text := string(body)
			if !strings.HasPrefix(text, "# "+d.ID+"\n") {
				t.Errorf("%s should open with the heading %q so the page and the id match", page, "# "+d.ID)
			}

			// The sections a reader needs before acting on a finding. The
			// "when this is wrong" section is the one that matters: a check
			// that cannot describe its own false positives should not be
			// recommending that anyone delete anything.
			for _, want := range []string{
				"## What the finding claims",
				"## How it is measured",
				"## What it does not mean",
				"## When this finding is wrong",
				"## What to do",
				"## Suppressing",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("%s is missing the %q section", page, want)
				}
			}

			if !strings.Contains(text, "ullage ignore "+d.ID+"/") {
				t.Errorf("%s should show a copyable `ullage ignore %s/...` command", page, d.ID)
			}
		})
	}
}

// The directory index is what GitHub renders when someone browses docs/checks,
// so a page that exists but is unreachable from it is effectively unpublished.
func TestChecksIndexLinksEveryPage(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("..", "..", "docs", "checks", "README.md"))
	if err != nil {
		t.Fatalf("reading the checks index: %v", err)
	}
	for _, c := range All() {
		name := c.Describe().ID
		if !strings.Contains(string(index), "]("+name+".md)") {
			t.Errorf("docs/checks/README.md does not link %s.md", name)
		}
	}
}

// A page for a check that no longer exists is worse than a missing one: it
// stays plausible for years and quietly documents behaviour nobody ships.
func TestNoOrphanedCheckPages(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "checks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	registered := map[string]bool{}
	for _, c := range All() {
		registered[c.Describe().ID] = true
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "README.md" {
			continue
		}
		if check := strings.TrimSuffix(name, ".md"); !registered[check] {
			t.Errorf("docs/checks/%s documents %q, which is not a registered check", name, check)
		}
	}
}
