package kube

import "testing"

// Kubernetes computes a pod's effective request as
//
//	max( sum(app) + sum(sidecars),
//	     max over init containers of ( sidecars started so far + this one ) )
//
// The second term is the one that used to be wrong: ordinary init containers
// were maxed on their own, ignoring the sidecars still running beside them.
// See staging/src/k8s.io/component-helpers/resource/helpers.go upstream.
func TestEffectiveRequestCountsSidecarsHeldDuringInit(t *testing.T) {
	gpu := func(n int) ResourceRequirements {
		if n == 0 {
			return ResourceRequirements{}
		}
		return ResourceRequirements{Limits: ResourceList{"nvidia.com/gpu": itoa(n)}}
	}

	cases := []struct {
		name string
		pod  *Pod
		want int
	}{{
		// The regression the auditors found: a 1-GPU sidecar is still running
		// when the 4-GPU init container starts, so five are held at once.
		name: "sidecar precedes a larger ordinary init",
		pod: &Pod{Spec: PodSpec{
			InitContainers: []Container{
				{Name: "sidecar", RestartPolicy: "Always", Resources: gpu(1)},
				{Name: "fetch", Resources: gpu(4)},
			},
		}},
		want: 5,
	}, {
		// Order matters: a sidecar declared after the init container has not
		// started yet, so nothing is held alongside it.
		name: "sidecar follows the ordinary init",
		pod: &Pod{Spec: PodSpec{
			InitContainers: []Container{
				{Name: "fetch", Resources: gpu(4)},
				{Name: "sidecar", RestartPolicy: "Always", Resources: gpu(1)},
			},
		}},
		// 4 while the init container runs, then 1 once only the sidecar is
		// left. The peak is 4, not 5: the sidecar had not started yet.
		want: 4,
	}, {
		name: "steady state wins when the app containers are larger",
		pod: &Pod{Spec: PodSpec{
			Containers:     []Container{{Name: "train", Resources: gpu(8)}},
			InitContainers: []Container{{Name: "fetch", Resources: gpu(4)}},
		}},
		want: 8,
	}, {
		name: "sidecars add to the steady state",
		pod: &Pod{Spec: PodSpec{
			Containers:     []Container{{Name: "train", Resources: gpu(4)}},
			InitContainers: []Container{{Name: "sidecar", RestartPolicy: "Always", Resources: gpu(2)}},
		}},
		want: 6,
	}, {
		name: "several sidecars accumulate before an ordinary init",
		pod: &Pod{Spec: PodSpec{
			InitContainers: []Container{
				{Name: "s1", RestartPolicy: "Always", Resources: gpu(1)},
				{Name: "s2", RestartPolicy: "Always", Resources: gpu(2)},
				{Name: "fetch", Resources: gpu(1)},
			},
		}},
		want: 4,
	}, {
		// Ordinary init containers run one at a time, so two of them never
		// overlap and the larger alone sets the floor.
		name: "ordinary init containers do not accumulate",
		pod: &Pod{Spec: PodSpec{
			InitContainers: []Container{
				{Name: "a", Resources: gpu(2)},
				{Name: "b", Resources: gpu(3)},
			},
		}},
		want: 3,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := tc.pod.GPURequest()
			if got != tc.want {
				t.Errorf("GPURequest() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A terminal pod has released its devices: every container has stopped, the
// kubelet has dropped it from the active set and the device manager has freed
// what it held. Reporting it as stuck would describe capacity that is already
// free, and a nonzero exit code is ordinary for a native sidecar.
func TestWedgedReasonIgnoresTerminalPods(t *testing.T) {
	crashed := []ContainerStatus{{
		Name:  "train",
		State: ContainerState{Terminated: &StateTerminated{ExitCode: 137, Reason: "Error"}},
	}}

	for _, phase := range []string{"Failed", "Succeeded"} {
		p := &Pod{Status: PodStatus{Phase: phase, ContainerStatuses: crashed}}
		if reason, _ := p.WedgedReason(); reason != "" {
			t.Errorf("phase %s: WedgedReason() = %q, want empty; the pod holds nothing", phase, reason)
		}
	}

	// The pods that genuinely do hold a device must still be caught. A pod
	// wedged on ImagePullBackOff is phase Pending and holds the accelerator
	// the scheduler already committed to it.
	running := &Pod{Status: PodStatus{Phase: "Running", ContainerStatuses: crashed}}
	if reason, _ := running.WedgedReason(); reason == "" {
		t.Error("a Running pod with a crashed container is no longer reported as wedged")
	}
	pending := &Pod{Status: PodStatus{Phase: "Pending", ContainerStatuses: []ContainerStatus{{
		Name:  "train",
		State: ContainerState{Waiting: &StateWaiting{Reason: "ImagePullBackOff"}},
	}}}}
	if reason, _ := pending.WedgedReason(); reason != "ImagePullBackOff" {
		t.Errorf("a bound Pending pod is no longer reported as wedged: %q", reason)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
