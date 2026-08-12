package kube

import "testing"

func gpuLimit(n string) ResourceRequirements {
	return ResourceRequirements{Limits: ResourceList{"nvidia.com/gpu": n}}
}

// A job that downloads model weights in an init container before training is
// an ordinary shape, and the GPU is often requested there. Ignoring init
// containers reported the pod as holding zero accelerators, which made it
// invisible to stuck-pod -- the check written to find exactly this -- and made
// the node under it look empty to unused-node.
//
// A pod wedged pulling a 40GB image, pinning an H100, was reported as nothing
// at all while the node it was on was offered up for deletion.
func TestInitContainerGPURequestCounts(t *testing.T) {
	p := &Pod{}
	p.Spec.InitContainers = []Container{{Name: "fetch-weights", Resources: gpuLimit("1")}}
	p.Spec.Containers = []Container{{Name: "train"}}

	got, res := p.GPURequest()
	if got != 1 {
		t.Fatalf("GPURequest = %d, want 1: the scheduler reserved a GPU for this pod's "+
			"init container, so it holds one whatever the app containers ask for", got)
	}
	if res != "nvidia.com/gpu" {
		t.Fatalf("resource = %q, want nvidia.com/gpu", res)
	}
}

// Kubernetes' effective request is max(sum(app), max(init)), not the sum of
// everything. Adding them would double-count a pod that requests a GPU in both
// places -- the init container has exited by the time the app container runs --
// and inflate every unused-hour figure derived from the count.
func TestInitAndAppRequestsTakeTheMaximumNotTheSum(t *testing.T) {
	p := &Pod{}
	p.Spec.InitContainers = []Container{{Name: "fetch", Resources: gpuLimit("1")}}
	p.Spec.Containers = []Container{{Name: "train", Resources: gpuLimit("4")}}

	if got, _ := p.GPURequest(); got != 4 {
		t.Fatalf("GPURequest = %d, want 4: the init container has exited by the time the "+
			"app container runs, so the pod never holds five", got)
	}
}

// A sidecar is an init container with restartPolicy: Always. It does not exit,
// so it runs alongside the app containers and its request adds to theirs.
func TestSidecarRequestAddsToTheAppContainers(t *testing.T) {
	p := &Pod{}
	p.Spec.InitContainers = []Container{
		{Name: "profiler", RestartPolicy: "Always", Resources: gpuLimit("1")},
	}
	p.Spec.Containers = []Container{{Name: "train", Resources: gpuLimit("4")}}

	if got, _ := p.GPURequest(); got != 5 {
		t.Fatalf("GPURequest = %d, want 5: a sidecar never exits, so it holds its "+
			"accelerator at the same time as the app containers hold theirs", got)
	}
}

// The same arithmetic has to apply to MIG and time-sliced requests, or a MIG
// pool running at capacity through init containers reads as empty.
func TestSliceRequestCountsInitContainers(t *testing.T) {
	p := &Pod{}
	p.Spec.InitContainers = []Container{{Name: "fetch", Resources: ResourceRequirements{
		Limits: ResourceList{"nvidia.com/mig-1g.5gb": "2"},
	}}}
	p.Spec.Containers = []Container{{Name: "train"}}

	if got, _ := p.SliceRequest(); got != 2 {
		t.Fatalf("SliceRequest = %d, want 2", got)
	}
}
