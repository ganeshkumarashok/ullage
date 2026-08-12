package main

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// The hero image on the front page states figures: a headline ratio, an
// accelerator census, a price, a finding id, and the command that frees it.
// Those came from a real `ullage demo` run, but an image is the one artefact
// nobody re-reads when a fixture changes, and a picture that disagrees with the
// tool is worse than no picture -- it is a confident claim that is wrong.
//
// So every figure the drawing asserts has to still appear in the transcripts,
// which are themselves checked against the binary. Change the demo cluster and
// this fails, naming the figure that has to be redrawn.

const heroPath = "../../docs/hero.svg"

func TestHeroImageIsWellFormed(t *testing.T) {
	raw, err := os.ReadFile(heroPath)
	if err != nil {
		t.Fatalf("read %s: %v", heroPath, err)
	}
	// A malformed SVG does not fail loudly; browsers render nothing, and the
	// front page silently loses its illustration.
	if err := xml.Unmarshal(raw, new(struct {
		XMLName xml.Name `xml:"svg"`
	})); err != nil {
		t.Fatalf("%s is not well-formed XML: %v", heroPath, err)
	}

	svg := string(raw)
	for _, want := range []string{
		// An image carrying the argument has to carry it to a screen reader too.
		`role="img"`,
		`<title`,
		`<desc`,
		// Motion is decorative; the argument must survive turning it off.
		"prefers-reduced-motion",
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("%s no longer contains %s", heroPath, want)
		}
	}
}

func TestHeroImageAgreesWithTheTranscripts(t *testing.T) {
	hero := mustRead(t, heroPath)
	readme := mustRead(t, "../../README.md")

	// Each of these is drawn in the image and printed by the tool. The README
	// transcripts are the checked copy of what the tool prints, so agreeing
	// with them is agreeing with the tool.
	for _, figure := range []string{
		"5.9k of 22k accelerator-hours unused (27%)",
		"60 of 68 accelerators analysed",
		"research/jupyter-alice",
		"~$3,427",
		"kubectl scale statefulset -n research jupyter-alice --replicas=0",
		"pool/l4-serving",
		"pool/h100-reserve",
		"2 pods block scale-down",
	} {
		if !strings.Contains(hero, figure) {
			t.Errorf("hero no longer shows %q -- if that is deliberate, drop it from this list", figure)
			continue
		}
		if !strings.Contains(readme, figure) {
			t.Errorf("hero shows %q but no transcript does; the picture has drifted from the tool", figure)
		}
	}

	// The one claim the picture makes that is about the tool rather than the
	// demo cluster, and the reason anyone runs it read-only.
	if !strings.Contains(hero, "never writes to your cluster") {
		t.Error("hero no longer states that ullage does not write to the cluster")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
