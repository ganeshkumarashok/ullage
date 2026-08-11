package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/config"
)

var now = time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)

func parse(t *testing.T, body string) *config.Suppressions {
	t.Helper()
	s, err := config.Parse([]byte(body), ".ullage.yaml", now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

func TestExactAndWildcardMatching(t *testing.T) {
	s := parse(t, `
suppress:
  - id: "idle-pod/research/jupyter-alice"
    reason: "on sabbatical"
  - id: "unused-node/pool/*"
    reason: "capacity reserved for launch"
`)
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"idle-pod/research/jupyter-alice", true},
		{"idle-pod/research/jupyter-bob", false},
		{"unused-node/pool/l4-serving", true},
		{"unused-node/pool/h100-reserve", true},
		{"stuck-pod/pool/l4-serving", false},
	} {
		if _, got := s.Match(tc.id); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// A `*` that crossed a slash would turn "this check, cluster-scoped" into
// "this check, every namespace in the cluster". The difference is a whole
// namespace of hidden findings, so it has to be asked for explicitly.
func TestWildcardDoesNotCrossSegments(t *testing.T) {
	s := parse(t, `
suppress:
  - id: "idle-pod/*"
    reason: "cluster-scoped only"
`)
	if _, ok := s.Match("idle-pod/research/jupyter-alice"); ok {
		t.Fatal("a single * matched across a path separator, silently widening the " +
			"suppression from one segment to a whole namespace")
	}
	if _, ok := s.Match("idle-pod/orphan"); !ok {
		t.Fatal("a single * should still match one segment")
	}
}

func TestExpiredEntriesStopApplyingAndSaySo(t *testing.T) {
	s := parse(t, `
suppress:
  - id: "idle-pod/research/jupyter-alice"
    reason: "temporary"
    until: "2026-08-09"
`)
	if _, ok := s.Match("idle-pod/research/jupyter-alice"); ok {
		t.Fatal("an expired suppression still applied; an expiry that renews itself is not one")
	}
	if !warned(s, "expired") {
		t.Fatal("an expired entry vanished silently, which is indistinguishable from " +
			"one that was never written")
	}
}

// The expiry names a day, so it is honoured through the end of that day rather
// than from its opening midnight.
func TestExpiryIsHonouredThroughItsLastDay(t *testing.T) {
	s := parse(t, `
suppress:
  - id: "idle-pod/a/b"
    reason: "today is the last day"
    until: "2026-08-11"
`)
	if _, ok := s.Match("idle-pod/a/b"); !ok {
		t.Fatal("an entry expiring today stopped applying at midnight")
	}
}

func TestUnmatchedEntriesAreReported(t *testing.T) {
	s := parse(t, `
suppress:
  - id: "idle-pod/research/typo"
    reason: "wrong name"
`)
	s.Match("idle-pod/research/real")
	if !warned(s, "matched nothing") {
		t.Fatal("a suppression that matched nothing was not reported; the reader believes " +
			"something is hidden that is not")
	}
}

func TestMatchedEntriesAreNotReportedAsDead(t *testing.T) {
	s := parse(t, `
suppress:
  - id: "idle-pod/research/real"
    reason: "known"
`)
	s.Match("idle-pod/research/real")
	if warned(s, "matched nothing") {
		t.Fatal("a working suppression was reported as dead")
	}
}

// Continuing past a broken file means printing findings the reader asked not to
// see, which they will read as "suppression is broken" rather than "my file is".
func TestMalformedFilesAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": "suppress:\n  - id: \"idle-pod/a/b\"\n    resaon: \"typo\"\n",
		"no reason":     "suppress:\n  - id: \"idle-pod/a/b\"\n",
		"no id":         "suppress:\n  - reason: \"orphan\"\n",
		"bad date":      "suppress:\n  - id: \"idle-pod/a/b\"\n    reason: \"x\"\n    until: \"31/12/2026\"\n",
		"not yaml":      "suppress: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Parse([]byte(body), ".ullage.yaml", now); err == nil {
				t.Fatal("accepted a file it cannot act on correctly")
			}
		})
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	s, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"), now)
	if err != nil {
		t.Fatalf("a cluster with no suppressions is the normal case: %v", err)
	}
	if s.Active() != 0 {
		t.Fatalf("Active() = %d, want 0", s.Active())
	}
}

func TestLoadReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".ullage.yaml")
	if err := os.WriteFile(p, []byte("suppress:\n  - id: \"idle-pod/a/b\"\n    reason: \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := s.Match("idle-pod/a/b"); !ok || reason != "x" {
		t.Fatalf("Match = %q, %v", reason, ok)
	}
}

// A nil receiver is the "no config" case throughout the scan, so it must behave
// like an empty list rather than panic.
func TestNilSuppressionsAreInert(t *testing.T) {
	var s *config.Suppressions
	if _, ok := s.Match("idle-pod/a/b"); ok {
		t.Fatal("nil suppressed something")
	}
	if s.Warnings() != nil || s.Active() != 0 {
		t.Fatal("nil should be empty, not surprising")
	}
}

func warned(s *config.Suppressions, substr string) bool {
	for _, w := range s.Warnings() {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
