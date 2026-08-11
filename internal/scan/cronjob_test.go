package scan

import (
	"strings"
	"testing"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Suspending a CronJob stops the next run. It does not touch the Job already
// running, which is the one holding the accelerators this finding is about.
//
// Presented as "the command", an operator runs it, watches the GPUs stay
// pinned, and concludes the tool does not work -- and a CI gate that applies
// the fix and re-scans reports it as having failed.
func TestCronJobFixDoesNotClaimToFreeCapacityNow(t *testing.T) {
	fix := SynthesiseFix(api.Provenance{
		Controlled: true, Recognized: true,
		RootKind: "CronJob", RootName: "nightly-eval",
	}, "research", []string{"nightly-eval-28123-abc"}, api.Owner{}, "", false)

	if fix.Command == "" {
		t.Fatal("no command at all: suspending is still the right way to stop this recurring")
	}
	if fix.Frees != api.FreesLater {
		t.Fatalf("Frees = %q, want %q. Suspending a CronJob frees nothing until the Job "+
			"already running finishes, and automation that applies this fix and re-scans "+
			"has no way to tell that from a fix that did not work.", fix.Frees, api.FreesLater)
	}
	if !strings.Contains(fix.Rationale, "frees nothing now") {
		t.Fatalf("the rationale does not tell the reader the capacity stays held: %q", fix.Rationale)
	}
}

// Every other controller fix does free capacity immediately, and marking them
// otherwise would make the distinction meaningless.
func TestOrdinaryControllerFixesFreeCapacityImmediately(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "Job"} {
		fix := SynthesiseFix(api.Provenance{
			Controlled: true, Recognized: true, RootKind: kind, RootName: "trainer",
		}, "research", []string{"trainer-0"}, api.Owner{}, "", false)
		if fix.Frees != "" {
			t.Fatalf("%s fix marked as freeing %q; scaling or deleting it releases the "+
				"accelerators at once", kind, fix.Frees)
		}
	}
}
