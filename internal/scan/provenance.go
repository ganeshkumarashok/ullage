package scan

import (
	"context"
	"fmt"
	"strings"

	"github.com/ullage-project/ullage/internal/kube"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Provenance resolution and fix synthesis.
//
// This is the precondition for a correct recommendation, not a detail. Without
// it the obvious remediation — kubectl delete pod — is a no-op for every
// controller-managed workload, which is most of them. The user runs the command
// the tool printed, the controller recreates the pod within seconds, the device
// is not freed, and the tool's headline recommendation visibly does nothing.
//
// The rule is absolute: resolve the owner reference chain to its root before
// printing any fix, and never print a command whose effect a controller will
// immediately undo.

// scalable kinds accept `kubectl scale`.
var scalableKinds = map[string]bool{
	"Deployment":            true,
	"StatefulSet":           true,
	"ReplicaSet":            true,
	"ReplicationController": true,
}

// recognisedKinds are the built-in workload kinds whose remediation semantics
// ullage is willing to assert. Everything else is a CRD whose meaning it does
// not know.
var recognisedKinds = map[string]bool{
	"Deployment":            true,
	"StatefulSet":           true,
	"ReplicaSet":            true,
	"ReplicationController": true,
	"DaemonSet":             true,
	"Job":                   true,
	"CronJob":               true,
}

// Resolver walks owner references, caching lookups because many pods share a
// controller.
type Resolver struct {
	client *kube.Client
	cache  map[string]*kube.Controller
}

func NewResolver(c *kube.Client) *Resolver {
	return &Resolver{client: c, cache: map[string]*kube.Controller{}}
}

// Resolve walks from a pod to the root of its ownership chain.
//
// The root matters, not the immediate parent: grouping or acting on a
// ReplicaSet would split one Deployment across every rollout it has ever had,
// and scaling a ReplicaSet is undone by its Deployment on the next reconcile.
func (r *Resolver) Resolve(ctx context.Context, pod *kube.Pod) api.Provenance {
	ref := pod.Controller()
	if ref == nil {
		return api.Provenance{
			Controlled: false,
			Recognised: true,
			RootKind:   "Pod",
			RootName:   pod.Metadata.Name,
		}
	}

	prov := api.Provenance{Controlled: true}
	kind, name, apiVersion := ref.Kind, ref.Name, ref.APIVersion
	namespace := pod.Metadata.Namespace

	prov.Chain = append(prov.Chain, api.OwnerRef{Kind: kind, Name: name, APIVersion: apiVersion})

	// Bounded walk: ownership chains are shallow, and a cycle in owner
	// references would otherwise hang the scan.
	for depth := 0; depth < 6; depth++ {
		obj, err := r.get(ctx, apiVersion, kind, namespace, name)
		if err != nil || obj == nil {
			// The object is gone or unreadable. What is known is still worth
			// reporting: the kind and name came from the owner reference.
			break
		}
		parent := controllerOf(obj.Metadata.OwnerReferences)
		if parent == nil {
			break
		}
		kind, name, apiVersion = parent.Kind, parent.Name, parent.APIVersion
		prov.Chain = append(prov.Chain, api.OwnerRef{Kind: kind, Name: name, APIVersion: apiVersion})
	}

	prov.RootKind = kind
	prov.RootName = name
	prov.APIVersion = apiVersion
	prov.Recognised = recognisedKinds[kind]
	return prov
}

func controllerOf(refs []kube.OwnerReference) *kube.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

func (r *Resolver) get(ctx context.Context, apiVersion, kind, namespace, name string) (*kube.Controller, error) {
	key := apiVersion + "|" + kind + "|" + namespace + "|" + name
	if obj, ok := r.cache[key]; ok {
		return obj, nil
	}
	obj, err := r.client.GetObject(ctx, apiVersion, kind, namespace, name)
	if err != nil {
		r.cache[key] = nil
		return nil, err
	}
	r.cache[key] = obj
	return obj, nil
}

// SynthesiseFix turns provenance into an action, or into an honest refusal.
//
// Three branches, and the third is the one that matters most. ullage does not
// know the semantics of an arbitrary CRD — deleting a Notebook may be routine
// or may be destructive — and guessing is how a tool earns a reputation for
// being dangerous. Naming the resource and declining to act on it is more
// useful than a confident wrong command.
func SynthesiseFix(prov api.Provenance, namespace string, pods []string, owner api.Owner, docs string) api.Fix {
	fix := api.Fix{
		RequiresHumanConfirmation: true,
		ConfirmWith:               owner.Identity,
		Prevention:                docs,
	}

	switch {
	case !prov.Controlled:
		fix.Targets = api.FixTargetPod
		fix.Rationale = "These pods have no controller, so deleting them frees the devices."
		fix.Command = deleteCommand(namespace, pods)

	case scalableKinds[prov.RootKind]:
		fix.Targets = api.FixTargetController
		fix.Rationale = fmt.Sprintf(
			"Deleting the pods will not free the devices — %s %s recreates them.",
			prov.RootKind, prov.RootName)
		fix.Command = fmt.Sprintf("kubectl scale %s -n %s %s --replicas=0",
			strings.ToLower(prov.RootKind), namespace, prov.RootName)

	case prov.RootKind == "Job":
		fix.Targets = api.FixTargetController
		fix.Rationale = "The Job owns these pods; deleting the pods alone would let the Job recreate them."
		fix.Command = fmt.Sprintf("kubectl delete job -n %s %s", namespace, prov.RootName)

	case prov.RootKind == "CronJob":
		fix.Targets = api.FixTargetController
		fix.Rationale = "A CronJob is producing these pods on a schedule; suspending it stops the next one."
		fix.Command = fmt.Sprintf(`kubectl patch cronjob -n %s %s -p '{"spec":{"suspend":true}}'`,
			namespace, prov.RootName)

	case prov.RootKind == "DaemonSet":
		fix.Targets = api.FixTargetNone
		fix.Rationale = fmt.Sprintf(
			"DaemonSet %s places a pod on every matching node. There is no safe scale-to-zero; "+
				"changing its nodeSelector or the node labels is the real remediation.", prov.RootName)

	default:
		// An unrecognised CRD. Refusing to guess is a stronger trust signal
		// than the command would have been.
		fix.Targets = api.FixTargetNone
		fix.Rationale = fmt.Sprintf(
			"These pods are owned by %s %s (%s), a custom resource whose deletion semantics ullage does not know. "+
				"Deleting the pods would not free the devices, and ullage will not guess how to remove the owner.",
			prov.RootKind, prov.RootName, prov.APIVersion)
	}
	return fix
}

func deleteCommand(namespace string, pods []string) string {
	if len(pods) == 0 {
		return ""
	}
	if len(pods) == 1 {
		return fmt.Sprintf("kubectl delete pod -n %s %s", namespace, pods[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "kubectl delete pod -n %s \\\n", namespace)
	for i, p := range pods {
		b.WriteString("    " + p)
		if i < len(pods)-1 {
			b.WriteString(" \\\n")
		}
	}
	return b.String()
}

// ownerLabelKeys are checked in order. Explicit ullage keys win, then the
// common conventions, so a cluster can opt into precise attribution without
// giving up the defaults.
var ownerLabelKeys = []string{
	"ullage.dev/owner",
	"owner",
	"app.kubernetes.io/owner",
	"team",
	"app.kubernetes.io/managed-by",
}

// AttributeOwner resolves who is responsible, and records how it was resolved.
//
// Showing the resolution path is what makes a wrong attribution debuggable
// instead of infuriating. Ownership never filters and never demotes a finding:
// `unowned` is a first-class, deliberately visible answer, because a device
// nobody claims is itself the finding.
func AttributeOwner(pod *kube.Pod, ctrl *kube.Controller, ns *kube.ObjectMeta) api.Owner {
	if o, via, detail := lookup(pod.Metadata.Labels, pod.Metadata.Annotations, "pod"); o != "" {
		return api.Owner{Identity: o, ResolvedVia: via, Detail: detail}
	}
	if ctrl != nil {
		if o, via, detail := lookup(ctrl.Metadata.Labels, ctrl.Metadata.Annotations, "workload"); o != "" {
			return api.Owner{Identity: o, ResolvedVia: via, Detail: detail}
		}
	}
	if ns != nil {
		if o, via, detail := lookup(ns.Labels, ns.Annotations, "namespace"); o != "" {
			return api.Owner{Identity: o, ResolvedVia: via, Detail: detail}
		}
	}
	return api.Owner{}
}

func lookup(labels, annotations map[string]string, scope string) (string, string, string) {
	for _, k := range ownerLabelKeys {
		if v, ok := annotations[k]; ok && v != "" {
			return v, scope + "-annotation", k + "=" + v
		}
		if v, ok := labels[k]; ok && v != "" {
			return v, scope + "-label", k + "=" + v
		}
	}
	// Contact annotations are common and unambiguous when present.
	for _, k := range []string{"ullage.dev/contact", "contact", "email"} {
		if v, ok := annotations[k]; ok && v != "" {
			return v, scope + "-annotation", k + "=" + v
		}
	}
	return "", "", ""
}

// OwnershipConfidence maps a resolved owner to its tier.
func OwnershipConfidence(o api.Owner) string {
	switch {
	case o.Identity == "":
		return api.OwnerUnowned
	case strings.HasSuffix(o.ResolvedVia, "-annotation") || strings.HasSuffix(o.ResolvedVia, "-label"):
		return api.OwnerResolved
	default:
		return api.OwnerInferred
	}
}
