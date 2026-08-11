// Package check defines what a check is and holds the built-in ones.
//
// A check is a pure detector. It reads the normalized fact layer and returns
// raw findings: what it saw, on which devices, and how confident it is in the
// measurement. It does not resolve owners, walk owner references, synthesise
// commands, apply prices, group, or rank — all of that happens once, downstream,
// for every check alike.
//
// That split is deliberate and it is the whole extensibility story. If a check
// had to populate a full finding it would need the Kubernetes client, the
// metrics client, the pricing table and the provenance resolver, and "add a
// check" would become a change across the entire system rather than one file.
// Here, a contributor implements three small methods, registers the check, and
// touches nothing else.
package check

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ullage-project/ullage/internal/inventory"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Params are the tunables a check may consult.
type Params struct {
	IdleThreshold  time.Duration
	StuckThreshold time.Duration

	// InitGrace is how long a pod may sit in init while holding devices before
	// it counts as stuck. Pulling a multi-gigabyte image or downloading model
	// weights routinely takes hours while legitimately holding the device.
	InitGrace time.Duration
}

// Descriptor is a check's self-description: what it looks for and where it is
// documented. The renderer and the docs both read this, so a new check gets
// consistent presentation for free.
type Descriptor struct {
	ID       string
	Title    string
	Question string

	// Claim is the precise thing the check asserts. Writing it down forces a
	// check author to be honest about what the evidence supports, and it is
	// what the explain screen shows the reader.
	Claim string

	// Risk is what could go wrong if the reader acts on it. Every check must
	// state one; a check with nothing to warn about is a check that has not
	// thought about being wrong.
	Risk       string
	Prevention string
	Docs       string
}

// Subject is what a finding is about: a workload, or a node pool.
type Subject struct {
	Kind      string // "workload" or "node-pool"
	Namespace string
	Name      string
	Pool      string

	// Pods are the pod references this finding covers. The pipeline uses these
	// to resolve provenance and owners, so a check never has to.
	Pods []inventory.PodRef

	// Nodes are the node names, for pool-scoped findings.
	Nodes []string
}

// RawFinding is what a check emits.
//
// Note what is absent: no owner, no fix, no command, no price, no rank. A check
// states what it measured; the pipeline decides what to do about it.
type RawFinding struct {
	Check   string
	Subject Subject

	// Devices are the device IDs the finding covers, which is how the pipeline
	// computes fallow hours without the check doing arithmetic.
	Devices []string

	// Fallow is how long the devices have been doing nothing.
	Fallow time.Duration

	Confidence string
	Evidence   api.Evidence
	Summary    string

	// ByDesign marks capacity that is empty on purpose, with Because saying
	// why. Such findings are reported but never counted as waste.
	ByDesign bool
	Because  string

	// Blockers are things preventing the obvious remediation from working.
	Blockers []api.Blocker
}

// Check is the extension point.
//
// Implement it, call Register in an init function, and the new check is picked
// up by the scanner, the --checks flag, the JSON contract and the docs with no
// other change anywhere.
type Check interface {
	// Describe returns the check's identity and its documented claim.
	Describe() Descriptor

	// Applicable reports whether this check can make a claim about a device.
	// Returning false is not the same as finding nothing: devices a check
	// cannot examine are counted in the not-analysed accounting, so the output
	// can always distinguish "clean" from "not looked at".
	Applicable(d inventory.Device) bool

	// Run detects. It must not mutate the cluster view.
	Run(ctx context.Context, c *inventory.Cluster, p Params) ([]RawFinding, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Check{}
)

// Register adds a check. Called from init functions.
func Register(c Check) {
	mu.Lock()
	defer mu.Unlock()
	id := c.Describe().ID
	if id == "" {
		panic("check registered without an ID")
	}
	if _, dup := registry[id]; dup {
		panic(fmt.Sprintf("check %q registered twice", id))
	}
	registry[id] = c
}

// All returns every registered check, in a stable order.
func All() []Check {
	mu.RLock()
	defer mu.RUnlock()
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Check, 0, len(ids))
	for _, id := range ids {
		out = append(out, registry[id])
	}
	return out
}

// Selected returns the named checks, or all of them when none are named.
func Selected(ids []string) ([]Check, error) {
	if len(ids) == 0 {
		return All(), nil
	}
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Check, 0, len(ids))
	for _, id := range ids {
		c, ok := registry[id]
		if !ok {
			return nil, fmt.Errorf("unknown check %q (known: %s)", id, known())
		}
		out = append(out, c)
	}
	return out, nil
}

// Lookup returns one check by ID.
func Lookup(id string) (Check, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[id]
	return c, ok
}

func known() string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	s := ""
	for i, id := range ids {
		if i > 0 {
			s += ", "
		}
		s += id
	}
	return s
}

// FindingID builds the stable cross-run reference for a finding.
//
// It is centralised because the ID is a documented contract: a user pastes it
// into a suppression file and expects it to still match next week. Deriving it
// ad hoc in each check is how two checks end up with two schemes.
func FindingID(checkID string, s Subject) string {
	switch s.Kind {
	case "node-pool":
		return checkID + "/pool/" + s.Pool
	default:
		if s.Namespace == "" {
			return checkID + "/" + s.Name
		}
		return checkID + "/" + s.Namespace + "/" + s.Name
	}
}
