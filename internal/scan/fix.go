package scan

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ullage-project/ullage/internal/check"
	"github.com/ullage-project/ullage/internal/inventory"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Provider renders the cloud-specific part of a remediation.
//
// It is a real seam rather than a switch statement because the difference
// between a correct `az aks nodepool update` and a plausible-looking wrong one
// is the difference between a tool people trust and one they stop running. It
// is also the natural place to hand a contributor ownership of a cloud they
// actually operate — the core carries no cloud SDK and never will.
type Provider interface {
	ID() string
	ScaleNodePool(pool string, min int) (command string, ok bool)
}

var (
	providerMu sync.RWMutex
	providers  = map[string]Provider{}
)

// RegisterProvider adds a cloud provider renderer.
//
// A duplicate ID panics rather than overwriting, matching check.Register. Last
// write wins was the worst possible behaviour for this seam: an integrator's
// provider would silently replace the built-in one, or be silently replaced by
// it, and the only symptom would be a remediation command that is confidently
// wrong for the cloud the user is actually running on.
func RegisterProvider(p Provider) {
	providerMu.Lock()
	defer providerMu.Unlock()
	id := p.ID()
	if id == "" {
		panic("provider registered without an ID")
	}
	if _, dup := providers[id]; dup {
		panic(fmt.Sprintf("provider %q registered twice", id))
	}
	providers[id] = p
}

func providerFor(id string) (Provider, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	p, ok := providers[id]
	return p, ok
}

func init() {
	RegisterProvider(azure{})
	RegisterProvider(aws{})
	RegisterProvider(gcp{})
}

type azure struct{}

func (azure) ID() string { return "azure" }
func (azure) ScaleNodePool(pool string, min int) (string, bool) {
	return fmt.Sprintf("az aks nodepool update -g <resource-group> --cluster-name <cluster> \\\n"+
		"    --name %s --min-count %d", pool, min), true
}

type aws struct{}

func (aws) ID() string { return "aws" }
func (aws) ScaleNodePool(pool string, min int) (string, bool) {
	return fmt.Sprintf("eksctl scale nodegroup --cluster <cluster> --name %s --nodes-min %d", pool, min), true
}

type gcp struct{}

func (gcp) ID() string { return "gcp" }
func (gcp) ScaleNodePool(pool string, min int) (string, bool) {
	return fmt.Sprintf("gcloud container clusters update <cluster> \\\n"+
		"    --node-pool %s --min-nodes %d", pool, min), true
}

// poolFix builds the remediation for an unused node pool.
//
// The blocker diagnosis comes first when it exists, because it is the actual
// answer: an empty node is visible in any dashboard, but "these two pods are
// why the autoscaler cannot reclaim it" is causal, is not visible anywhere
// else, and is safe to act on.
func poolFix(cl *inventory.Cluster, rf check.RawFinding, desc check.Descriptor) api.Fix {
	fix := api.Fix{
		RequiresHumanConfirmation: true,
		Targets:                   api.FixTargetNodePool,
		Blockers:                  rf.Blockers,
		Prevention:                desc.Prevention,
	}

	if len(rf.Blockers) > 0 {
		fix.Rationale = fmt.Sprintf(
			"%d pods prevent the autoscaler from draining these nodes. Scaling the pool down "+
				"while they are there will not work — resolve them first, and the autoscaler "+
				"will reclaim the capacity on its own.", len(rf.Blockers))
		fix.Targets = api.FixTargetPod
		if ns, name, ok := strings.Cut(rf.Blockers[0].Object, "/"); ok {
			fix.Command = fmt.Sprintf(
				"kubectl get pod -n %s %s -o yaml | grep -A2 safe-to-evict", ns, name)
		}
		return fix
	}

	// Karpenter first, because it wins over the cloud CLI wherever both are
	// present. A Karpenter NodePool is not an ASG or a VMSS: `eksctl scale
	// nodegroup --name <nodepool>` names an object that does not exist, and if
	// a similarly named node group *does* exist the command silently acts on
	// the wrong thing. Karpenter also has no minimum size to lower, so the only
	// honest suggestion is to look at what is stopping consolidation.
	// Decided per node, then falling back to autoscaler discovery. A node that
	// says it is Karpenter's is Karpenter's, regardless of what else is
	// installed or what the scan was permitted to read.
	karpenter := cl.Autoscaler.Reclaims()
	for _, name := range rf.Subject.Nodes {
		if n := cl.NodeByName(name); n != nil && n.Karpenter() {
			karpenter = true
			break
		}
	}
	if karpenter {
		fix.Rationale = "Karpenter manages this pool and has no minimum size, so there is no floor " +
			"to lower — it should have consolidated these nodes already. Something is preventing " +
			"that: a disruption budget, a do-not-disrupt annotation, or a pod Karpenter will not " +
			"move. Nothing here needs scaling down; find what is holding it."
		fix.Command = fmt.Sprintf(
			"kubectl get nodepool %s -o yaml   # check spec.disruption", rf.Subject.Pool)
		return fix
	}

	provider := ""
	for _, name := range rf.Subject.Nodes {
		if n := cl.NodeByName(name); n != nil {
			provider = n.Provider
			break
		}
	}
	if p, ok := providerFor(provider); ok {
		if cmd, ok := p.ScaleNodePool(rf.Subject.Pool, 0); ok {
			fix.Command = cmd
			fix.Rationale = "Nothing blocks scale-down, so the pool's minimum size is holding these " +
				"nodes. Lowering the floor lets the autoscaler reclaim them."
			return fix
		}
	}
	fix.Rationale = "Nothing blocks scale-down. ullage does not recognise this cluster's provider, " +
		"so it will not guess the command — scale the pool with whatever manages it."
	fix.Command = fmt.Sprintf("# scale node pool %q to a minimum of 0 using its manager\n"+
		"# (Karpenter NodePool, Cluster API MachineDeployment, or your cloud CLI)", rf.Subject.Pool)
	return fix
}

// stuckFix points at the logs rather than at a deletion.
//
// A crash loop is never intentional, so the useful next step is diagnosis. A
// tool that suggests deleting a crash-looping pod is suggesting the reader
// destroy the evidence.
func stuckFix(pod inventory.PodView, rf check.RawFinding, desc check.Descriptor) api.Fix {
	fix := api.Fix{
		Targets:     api.FixTargetPod,
		ConfirmWith: pod.Owner.Identity,
		Prevention:  desc.Prevention,
		Command: fmt.Sprintf("kubectl logs -n %s %s --previous",
			pod.Ref.Namespace, pod.Ref.Name),
	}
	fix.Rationale = "The devices stay allocated for as long as this continues. Start with the " +
		"previous container's logs; the fix belongs to whoever owns the workload."
	if pod.Terminated != nil {
		fix.Rationale = fmt.Sprintf("Last exit was %d (%s). %s",
			pod.Terminated.ExitCode, pod.Terminated.Reason, fix.Rationale)
	}
	return fix
}
