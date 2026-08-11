// Package config reads .ullage.yaml, the file that records which findings a
// team has decided not to act on.
//
// Suppression is the feature that decides whether a tool like this survives its
// second month. Every scanner produces findings its owners have consciously
// accepted, and a tool with no way to record that decision gets one of two
// responses: it gets muted entirely, or somebody wraps it in a grep. Both lose
// the decision, so the next person rediscovers the finding and reopens the
// argument.
//
// Three rules follow from that, and they are the whole design:
//
//   - A suppression that matches nothing is reported. It means either a typo,
//     in which case the reader believes something is hidden that is not, or a
//     genuinely fixed problem, in which case the entry is litter. Silence would
//     be wrong for both.
//   - An expired suppression stops applying and says so by name. An expiry that
//     quietly renews itself is not an expiry.
//   - A malformed file is a hard error. Continuing with an unparsed suppression
//     list means printing findings the reader asked not to see, and they will
//     read the output as "the suppression did not work" rather than "the file
//     is broken".
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the file consulted when none is named.
const DefaultPath = ".ullage.yaml"

// Suppression is one accepted finding.
type Suppression struct {
	// ID matches against api.Finding.ID. Finding IDs are slash-separated
	// paths — check/namespace/name — so `*` is supported per segment, which
	// makes "everything this team owns" expressible without listing it:
	//
	//	idle-allocation/team-a/*   one check, one namespace
	//	*/team-a/*                 every check, one namespace
	ID string `yaml:"id"`

	// Reason is required. A suppression without one is indistinguishable from
	// a mistake six months later, and the person who has to judge it is
	// usually not the person who wrote it.
	Reason string `yaml:"reason"`

	// Until is optional and, when set, is the date the entry stops applying.
	Until string `yaml:"until,omitempty"`
}

// File is the parsed contents of .ullage.yaml.
type File struct {
	Suppress []Suppression `yaml:"suppress"`
}

// Suppressions is a compiled, queryable suppression list.
type Suppressions struct {
	path    string
	entries []Suppression
	// expired holds entries whose Until has passed. They are kept rather than
	// dropped so the scan can name them: an entry that silently stopped
	// working looks identical to one that was never written.
	expired []Suppression
	// used records which patterns actually matched a finding, so the ones that
	// did not can be reported.
	used map[string]bool
}

// Load reads a suppression file. A missing file is not an error — most clusters
// will never have one — but an unreadable or malformed one is.
func Load(p string, now time.Time) (*Suppressions, error) {
	if p == "" {
		p = DefaultPath
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Suppressions{path: p, used: map[string]bool{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	return Parse(raw, p, now)
}

// Parse compiles a suppression file that has already been read.
func Parse(raw []byte, p string, now time.Time) (*Suppressions, error) {
	var f File
	// KnownFields would be better still, but yaml.v3 only exposes it on a
	// Decoder. A field the tool does not understand is far more likely to be a
	// misspelling of one it does than a deliberate extension, and a silently
	// ignored `resaon:` is a suppression with no reason at all.
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && err.Error() != "EOF" {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}

	s := &Suppressions{path: p, used: map[string]bool{}}
	for i, e := range f.Suppress {
		switch {
		case e.ID == "":
			return nil, fmt.Errorf("%s: suppress[%d] has no id", p, i)
		case e.Reason == "":
			return nil, fmt.Errorf("%s: suppress[%d] (%s) has no reason; "+
				"an unexplained suppression cannot be reviewed later", p, i, e.ID)
		}
		if _, err := path.Match(e.ID, "x"); err != nil {
			return nil, fmt.Errorf("%s: suppress[%d] id %q is not a valid pattern: %w", p, i, e.ID, err)
		}
		if e.Until != "" {
			t, err := time.Parse("2006-01-02", e.Until)
			if err != nil {
				return nil, fmt.Errorf("%s: suppress[%d] (%s) has until %q, want YYYY-MM-DD",
					p, i, e.ID, e.Until)
			}
			// The expiry names a day, so it is honoured through the end of it.
			if now.After(t.AddDate(0, 0, 1)) {
				s.expired = append(s.expired, e)
				continue
			}
		}
		s.entries = append(s.entries, e)
	}
	return s, nil
}

// Match reports whether a finding ID is suppressed, and why.
func (s *Suppressions) Match(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, e := range s.entries {
		if matches(e.ID, id) {
			s.used[e.ID] = true
			return e.Reason, true
		}
	}
	return "", false
}

// matches compares a pattern against a finding ID segment by segment.
//
// Segment-wise rather than whole-string so that `*` cannot cross a `/`. A
// pattern of `idle-allocation/*` should mean "cluster-scoped findings from this
// check", not "every finding from this check in every namespace" — the second
// meaning is available as `idle-allocation/*/*` and should have to be asked for,
// because the difference is a whole namespace of hidden findings.
func matches(pattern, id string) bool {
	if pattern == id {
		return true
	}
	pp, ip := strings.Split(pattern, "/"), strings.Split(id, "/")
	if len(pp) != len(ip) {
		return false
	}
	for i := range pp {
		ok, err := path.Match(pp[i], ip[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// Warnings describes anything about the file the reader needs to know: entries
// that have expired, and entries that matched nothing.
//
// Returned as warnings rather than printed here so they travel in the JSON
// result too. A team running this in CI never sees the terminal.
func (s *Suppressions) Warnings() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, e := range s.expired {
		out = append(out, fmt.Sprintf(
			"suppression %q in %s expired on %s and no longer applies; remove it or extend --until",
			e.ID, s.path, e.Until))
	}
	var unused []string
	for _, e := range s.entries {
		if !s.used[e.ID] {
			unused = append(unused, e.ID)
		}
	}
	sort.Strings(unused)
	for _, id := range unused {
		out = append(out, fmt.Sprintf(
			"suppression %q in %s matched nothing; either the finding is fixed and the entry "+
				"can be deleted, or the id is wrong and nothing is being suppressed", id, s.path))
	}
	return out
}

// Active reports how many entries are in force.
func (s *Suppressions) Active() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}
