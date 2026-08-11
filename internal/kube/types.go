package kube

import (
	"strconv"
	"strings"
	"time"
)

// The subset of the Kubernetes API this tool reads. Hand-written rather than
// pulled from client-go: ullage reads six resource types and never writes, so
// the full API machinery would be several hundred megabytes of dependency for
// no benefit. Everything here is plain JSON over HTTPS.

// ResourceNvidiaGPU is the classic device-plugin extended resource. Under DRA
// this key is absent entirely, which is why its absence is load-bearing.
const ResourceNvidiaGPU = "nvidia.com/gpu"

// KnownGPUResources are the extended resources treated as accelerators. Vendor
// neutrality is a hard requirement for the project, so this is a list rather
// than a constant.
var KnownGPUResources = []string{
	"nvidia.com/gpu",
	"amd.com/gpu",
	"gpu.intel.com/i915",
	"habana.ai/gaudi",
}

type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller,omitempty"`
}

type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences"`
	GenerateName      string            `json:"generateName"`
}

type ResourceList map[string]string

// GPUs returns the accelerator count in a resource list and the resource name
// that carried it.
func (r ResourceList) GPUs() (int, string) {
	for _, key := range KnownGPUResources {
		if v, ok := r[key]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil && n > 0 {
				return n, key
			}
		}
	}
	return 0, ""
}

type ResourceRequirements struct {
	Limits   ResourceList `json:"limits"`
	Requests ResourceList `json:"requests"`
}

type Container struct {
	Name      string               `json:"name"`
	Image     string               `json:"image"`
	Resources ResourceRequirements `json:"resources"`
}

type PodResourceClaim struct {
	Name              string `json:"name"`
	ResourceClaimName string `json:"resourceClaimName,omitempty"`
}

type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

type PodSpec struct {
	NodeName       string             `json:"nodeName"`
	Containers     []Container        `json:"containers"`
	InitContainers []Container        `json:"initContainers"`
	ResourceClaims []PodResourceClaim `json:"resourceClaims"`
	Tolerations    []Toleration       `json:"tolerations"`
	Priority       *int               `json:"priority,omitempty"`
}

type StateWaiting struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type StateRunning struct {
	StartedAt time.Time `json:"startedAt"`
}

type StateTerminated struct {
	Reason     string    `json:"reason"`
	ExitCode   int       `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt"`
	Message    string    `json:"message"`
}

type ContainerState struct {
	Waiting    *StateWaiting    `json:"waiting,omitempty"`
	Running    *StateRunning    `json:"running,omitempty"`
	Terminated *StateTerminated `json:"terminated,omitempty"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type PodStatus struct {
	Phase                 string            `json:"phase"`
	StartTime             *time.Time        `json:"startTime,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
	Conditions            []PodCondition    `json:"conditions"`
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status"`
}

// GPURequest returns the total accelerators requested by a pod's containers.
// Init containers are excluded: their requests are maxed, not summed, and they
// do not add to the pod's steady-state allocation.
func (p *Pod) GPURequest() (int, string) {
	total, resource := 0, ""
	for _, c := range p.Spec.Containers {
		n, res := c.Resources.Limits.GPUs()
		if n == 0 {
			n, res = c.Resources.Requests.GPUs()
		}
		if n > 0 {
			total += n
			resource = res
		}
	}
	return total, resource
}

// UsesDRA reports whether the pod obtains devices through ResourceClaims. Under
// DRA no extended resource appears in requests at all, so a scan keyed on
// nvidia.com/gpu would silently see nothing.
func (p *Pod) UsesDRA() bool { return len(p.Spec.ResourceClaims) > 0 }

// Controller returns the controlling owner reference, if any.
func (p *Pod) Controller() *OwnerReference {
	for i := range p.Metadata.OwnerReferences {
		or := p.Metadata.OwnerReferences[i]
		if or.Controller != nil && *or.Controller {
			return &p.Metadata.OwnerReferences[i]
		}
	}
	if len(p.Metadata.OwnerReferences) > 0 {
		return &p.Metadata.OwnerReferences[0]
	}
	return nil
}

// WedgedReason returns a description of why a pod is stuck holding a device,
// plus the terminated state that explains it. Pending is deliberately not a
// wedged state: a Pending pod holds nothing.
func (p *Pod) WedgedReason() (string, *StateTerminated) {
	all := append(append([]ContainerStatus{}, p.Status.ContainerStatuses...), p.Status.InitContainerStatuses...)
	for _, cs := range all {
		if cs.State.Waiting != nil {
			switch cs.State.Waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerError", "RunContainerError":
				return cs.State.Waiting.Reason, cs.LastState.Terminated
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return cs.State.Terminated.Reason, cs.State.Terminated
		}
	}
	return "", nil
}

// RestartCount is the highest restart count across the pod's containers.
func (p *Pod) RestartCount() int {
	max := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > max {
			max = cs.RestartCount
		}
	}
	return max
}

type NodeSpec struct {
	Unschedulable bool    `json:"unschedulable"`
	Taints        []Taint `json:"taints"`
	ProviderID    string  `json:"providerID"`
}

type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

type NodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type NodeStatus struct {
	Allocatable ResourceList    `json:"allocatable"`
	Capacity    ResourceList    `json:"capacity"`
	Conditions  []NodeCondition `json:"conditions"`
	NodeInfo    NodeSystemInfo  `json:"nodeInfo"`
}

type NodeSystemInfo struct {
	KubeletVersion string `json:"kubeletVersion"`
}

type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     NodeSpec   `json:"spec"`
	Status   NodeStatus `json:"status"`
}

// Ready reports whether the node's Ready condition is True.
func (n *Node) Ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// Pool returns the node pool or node group name, trying the label keys used by
// the major providers before falling back to the node name.
func (n *Node) Pool() string {
	for _, k := range []string{
		"agentpool",
		"kubernetes.azure.com/agentpool",
		"eks.amazonaws.com/nodegroup",
		"cloud.google.com/gke-nodepool",
		"karpenter.sh/nodepool",
		"node.kubernetes.io/instancegroup",
	} {
		if v, ok := n.Metadata.Labels[k]; ok && v != "" {
			return v
		}
	}
	return n.Metadata.Name
}

// Provider infers the cloud provider from the providerID or node labels, which
// is what selects the provider plugin that renders cloud-specific commands.
// The core carries no cloud SDK.
func (n *Node) Provider() string {
	switch {
	case strings.HasPrefix(n.Spec.ProviderID, "azure://"):
		return "azure"
	case strings.HasPrefix(n.Spec.ProviderID, "aws://"):
		return "aws"
	case strings.HasPrefix(n.Spec.ProviderID, "gce://"):
		return "gcp"
	default:
		return "unknown"
	}
}

// GPUModel returns the accelerator product name advertised by the GPU operator
// feature discovery labels.
func (n *Node) GPUModel() string {
	for _, k := range []string{
		"nvidia.com/gpu.product",
		"gpu.intel.com/product",
		"amd.com/gpu.product",
		"accelerator",
	} {
		if v, ok := n.Metadata.Labels[k]; ok && v != "" {
			return v
		}
	}
	return "unknown"
}

type PodDisruptionBudget struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		MinAvailable   any            `json:"minAvailable"`
		MaxUnavailable any            `json:"maxUnavailable"`
		Selector       *LabelSelector `json:"selector"`
	} `json:"spec"`
	Status struct {
		DisruptionsAllowed int `json:"disruptionsAllowed"`
	} `json:"status"`
}

// LabelSelector is the matchLabels subset. matchExpressions is not supported:
// a partial implementation that silently ignores expressions would claim a PDB
// does not cover a pod when it does, which turns a blocked scale-down into a
// recommendation to scale down.
type LabelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

// Matches reports whether a label set satisfies the selector, and whether the
// answer is trustworthy. An unsupported matchExpressions clause returns
// ok=false so the caller can say "unknown" rather than "no".
func (s *LabelSelector) Matches(labels map[string]string) (matched, ok bool) {
	if s == nil {
		return false, true
	}
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false, true
		}
	}
	if len(s.MatchExpressions) > 0 {
		return len(s.MatchLabels) > 0, false
	}
	return len(s.MatchLabels) > 0, true
}

// ResourceClaim is the DRA allocation object. ullage does not interpret it in
// v1; it counts it, so that DRA-allocated devices are reported as unexamined
// rather than silently omitted.
type ResourceClaim struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Allocation *struct {
			Devices struct {
				Results []struct {
					Device string `json:"device"`
					Driver string `json:"driver"`
					Pool   string `json:"pool"`
				} `json:"results"`
			} `json:"devices"`
		} `json:"allocation,omitempty"`
		ReservedFor []struct {
			Resource string `json:"resource"`
			Name     string `json:"name"`
			UID      string `json:"uid"`
		} `json:"reservedFor"`
	} `json:"status"`
}

// Generic controller object: the fields ullage needs are the same for every
// workload kind, and unknown CRDs are read with exactly this shape.
type Controller struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       struct {
		Replicas *int `json:"replicas,omitempty"`
	} `json:"spec"`
}

type list[T any] struct {
	Items    []T `json:"items"`
	Metadata struct {
		// Continue is set when the API server truncated the response. Ignoring
		// it silently returns a partial cluster, and a partial cluster is how a
		// tool reports a node as empty because the pods on it were on page two.
		Continue string `json:"continue"`
	} `json:"metadata"`
}

// APIResource is one entry from a discovery document.
type APIResource struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Namespaced bool   `json:"namespaced"`
}

type apiResourceList struct {
	GroupVersion string        `json:"groupVersion"`
	Resources    []APIResource `json:"resources"`
}

type versionInfo struct {
	GitVersion string `json:"gitVersion"`
	Major      string `json:"major"`
	Minor      string `json:"minor"`
}
