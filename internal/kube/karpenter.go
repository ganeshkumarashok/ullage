package kube

import (
	"context"
	"errors"
	"strings"
)

// Karpenter is the second way a Kubernetes cluster decides how many nodes to
// keep, and on AWS it is now the more common one. It matters here because
// ullage's central caution — "an empty pool may be held at a deliberate
// minimum, so do not call it waste" — is a cluster-autoscaler concept that has
// no Karpenter equivalent.
//
// A Karpenter NodePool has no minimum size. Karpenter consolidates empty nodes
// on its own, so an empty Karpenter node is *never* a deliberate reservation;
// it is either about to disappear or something is stopping it. That makes the
// finding stronger, not weaker, and it means the "no autoscaler status was
// readable" caveat must not be printed on a Karpenter cluster — where it would
// appear on every finding and be wrong every time.
//
// What Karpenter does have is disruption controls, and those are real blockers:
// a NodePool whose disruption budget allows zero nodes is deliberately pinned,
// and a pod or node annotated karpenter.sh/do-not-disrupt cannot be reclaimed.
type Karpenter struct {
	// NodePools is keyed by NodePool name.
	NodePools map[string]NodePool
}

type NodePool struct {
	Name string

	// Pinned is set when an unconditional disruption budget allows zero nodes,
	// which is the closest Karpenter analogue of a minimum size: the operator
	// has said this pool may not shrink.
	Pinned bool

	// ScheduledHold notes a budget that allows zero nodes only during a
	// schedule. ullage does not evaluate cron windows, so this is surfaced as
	// context rather than treated as a hold.
	ScheduledHold bool
}

// NodePoolOf returns the Karpenter NodePool a node belongs to, if any.
func NodePoolOf(n *Node) (string, bool) {
	for _, key := range []string{"karpenter.sh/nodepool", "karpenter.sh/provisioner-name"} {
		if v := n.Metadata.Labels[key]; v != "" {
			return v, true
		}
	}
	return "", false
}

// DoNotDisrupt reports whether an object carries Karpenter's opt-out
// annotation, which prevents Karpenter from reclaiming it or the node under it.
func DoNotDisrupt(m ObjectMeta) bool {
	return strings.EqualFold(m.Annotations["karpenter.sh/do-not-disrupt"], "true") ||
		strings.EqualFold(m.Annotations["karpenter.sh/do-not-evict"], "true") ||
		strings.EqualFold(m.Annotations["karpenter.sh/do-not-consolidate"], "true")
}

type nodePoolDoc struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Disruption struct {
			Budgets []struct {
				Nodes    string   `json:"nodes"`
				Schedule string   `json:"schedule"`
				Reasons  []string `json:"reasons"`
			} `json:"budgets"`
		} `json:"disruption"`
	} `json:"spec"`
}

// KarpenterNodePools lists Karpenter NodePools, newest API group first.
//
// Absence is the common case and is not an error: it means the cluster does not
// run Karpenter, and the caller should fall back to cluster-autoscaler status.
// A nil return with a nil error means "no Karpenter here".
func (c *Client) KarpenterNodePools(ctx context.Context) (*Karpenter, error) {
	for _, gv := range []string{"karpenter.sh/v1", "karpenter.sh/v1beta1"} {
		items, err := listAll[nodePoolDoc](ctx, c, "/apis/"+gv+"/nodepools")
		if err != nil {
			var nf *NotFound
			if isNotFound(err, &nf) {
				continue
			}
			var forbidden *Forbidden
			if errors.As(err, &forbidden) {
				continue
			}
			return nil, err
		}
		k := &Karpenter{NodePools: map[string]NodePool{}}
		for _, np := range items {
			p := NodePool{Name: np.Metadata.Name}
			for _, b := range np.Spec.Disruption.Budgets {
				if !zeroNodeBudget(b.Nodes) {
					continue
				}
				if !budgetBlocksReclaim(b.Reasons) {
					// A budget scoped to reasons that have nothing to do with
					// reclaiming an idle node does not hold it. Treating a
					// Drifted-only budget as a pin suppresses the finding on
					// exactly the clusters that configured Karpenter carefully
					// enough to scope their budgets.
					continue
				}
				if strings.TrimSpace(b.Schedule) == "" {
					p.Pinned = true
				} else {
					p.ScheduledHold = true
				}
			}
			k.NodePools[np.Metadata.Name] = p
		}
		if len(k.NodePools) == 0 {
			// The CRD is installed but no NodePool exists. Karpenter is not
			// managing anything, so treat it as absent rather than claim
			// knowledge of a cluster it does not run.
			return nil, nil
		}
		return k, nil
	}
	return nil, nil
}

// zeroNodeBudget reports whether a disruption budget forbids all disruption.
// The field is a string holding either a count or a percentage.
func zeroNodeBudget(v string) bool {
	v = strings.TrimSpace(v)
	return v == "0" || v == "0%"
}

// budgetBlocksReclaim reports whether a disruption budget's reasons cover the
// way Karpenter would remove an idle node.
//
// Karpenter disrupts for Underutilized, Empty and Drifted. Only the first two
// are how an idle GPU node goes away; a budget scoped to Drifted pins node
// replacement during an AMI rollout and says nothing about consolidation. An
// empty reasons list means every reason, which is the common case and the one
// that does hold.
func budgetBlocksReclaim(reasons []string) bool {
	if len(reasons) == 0 {
		return true
	}
	for _, r := range reasons {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "underutilized", "empty":
			return true
		}
	}
	return false
}
