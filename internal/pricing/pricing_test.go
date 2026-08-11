package pricing_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshkumarashok/ullage/internal/pricing"
	"github.com/ganeshkumarashok/ullage/pkg/ullage/api"
)

// Load("") is what every scan uses unless a user passes --pricing, so the
// built-in table has to actually parse, and the source string has to say
// plainly that these are approximations — that string is the only thing
// standing between a reader and mistaking a rough list price for a quote.
func TestLoadDefaultsParseAndDisclaimTheirOwnAccuracy(t *testing.T) {
	p, err := pricing.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") returned %v; the built-in rates ship broken", err)
	}
	if p.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", p.Currency)
	}
	if !strings.Contains(strings.ToLower(p.Source), "approximate") {
		t.Fatalf("Source = %q; a reader has no way to know this number is not authoritative", p.Source)
	}
	if len(p.PerSKUGPUHour) == 0 {
		t.Fatal("PerSKUGPUHour is empty; every finding would report no cost")
	}
}

// A wrong price silently misstates the headline dollar figure, so the exact
// rate for a known SKU is worth pinning down, not just checking "some value
// came back".
func TestDefaultRateForAKnownSKUMatchesThePublishedList(t *testing.T) {
	p, err := pricing.Load("")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := p.Rate("NVIDIA-H100-SXM5-80GB")
	if !ok {
		t.Fatal("Rate(NVIDIA-H100-SXM5-80GB) ok=false, want a hit against the built-in list")
	}
	if got != 5.50 {
		t.Fatalf("Rate(NVIDIA-H100-SXM5-80GB) = %v, want 5.50", got)
	}
}

// A miss must be an explicit, checkable "no rate known", not a coerced zero
// that looks identical to "this GPU costs nothing".
func TestRateMissReturnsFalseNotASilentZero(t *testing.T) {
	p, err := pricing.Load("")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := p.Rate("a-model-that-does-not-exist")
	if ok {
		t.Fatalf("Rate for an unknown model reported ok=true (value %v); a made-up SKU must never look priced", got)
	}
	if got != 0 {
		t.Fatalf("Rate for an unknown model = %v with ok=false; want the zero value alongside the false", got)
	}
}

// Rate does no case-folding or normalisation: the model string is looked up
// verbatim against the map key. This is worth locking in explicitly, because
// dcgm-exporter's device names and a hand-edited --pricing file could easily
// disagree in case, and the silent failure mode (ok=false, cost omitted) looks
// identical to "no pricing file configured" rather than "your file has a typo".
func TestRateDoesNotNormaliseModelNameCase(t *testing.T) {
	p, err := pricing.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Rate("nvidia-h100-sxm5-80gb"); ok {
		t.Fatal("Rate matched a lower-cased model name; lookups are documented as verbatim, so this either changed silently or the map has a duplicate")
	}
}

// A nil *Pricing means "no pricing information was ever loaded". It must
// behave like a complete miss rather than panicking, because callers on the
// no-pricing-configured path hold a nil pointer, not an empty struct.
func TestRateOnNilPricingIsATotalMissNotAPanic(t *testing.T) {
	var p *api.Pricing
	got, ok := p.Rate("anything")
	if ok || got != 0 {
		t.Fatalf("Rate on a nil *Pricing = (%v, %v), want (0, false)", got, ok)
	}
}

// Ullage never blends rates across models — a T4 and an H100 differ roughly
// tenfold, and averaging them would fabricate a number with a decimal point
// that looks precise. Rate has no blending code path at all: each call is one
// model in, one rate out, so two different SKUs must never collapse to the
// same figure by way of shared state or a running average.
func TestRateNeverBlendsAcrossDifferentModels(t *testing.T) {
	p, err := pricing.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h100, ok := p.Rate("NVIDIA-H100-SXM5-80GB")
	if !ok {
		t.Fatal("expected a rate for NVIDIA-H100-SXM5-80GB")
	}
	t4, ok := p.Rate("Tesla-T4")
	if !ok {
		t.Fatal("expected a rate for Tesla-T4")
	}
	if h100 == t4 {
		t.Fatalf("H100 and T4 both priced at %v; a shared or blended rate would misstate the cost of every mixed-GPU finding by up to 10x", h100)
	}
	// Calling Rate for one model must not perturb what a later call for a
	// different model returns — there is no per-call mutable state to leak
	// between them.
	again, _ := p.Rate("NVIDIA-H100-SXM5-80GB")
	if again != h100 {
		t.Fatalf("Rate(H100) returned %v then %v across two calls with a T4 lookup in between", h100, again)
	}
}

// perGPUHour is the fallback flat rate used when a cluster's GPUs are not (or
// not all) in the per-SKU table. A SKU-specific entry must win over it when
// both are present, because a specific number is always better evidence than
// a generic fallback.
func TestPerSKURateWinsOverTheFlatFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rates.yaml")
	yaml := "perGPUHour: 1.00\nperSKUGPUHour:\n  Widget-9000: 9.99\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p.Rate("Widget-9000"); !ok || got != 9.99 {
		t.Fatalf("Rate(Widget-9000) = (%v, %v), want (9.99, true); the specific rate must win over the flat fallback", got, ok)
	}
	if got, ok := p.Rate("Some-Other-Model"); !ok || got != 1.00 {
		t.Fatalf("Rate(Some-Other-Model) = (%v, %v), want (1.00, true) from the flat fallback", got, ok)
	}
}

// A custom file that has neither a per-SKU entry nor a positive perGPUHour
// leaves Rate with nothing to return: it must report a miss, not fabricate a
// zero-cost rate that renders as free hardware.
func TestNoRateConfiguredIsAMissNotAFreeZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, []byte("currency: USD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p.Rate("anything"); ok {
		t.Fatalf("Rate with no configured rate returned (%v, true); an unpriced cluster must show no cost, not a free one", got)
	}
}

func TestLoadReadsACustomFileAndNamesItAsTheSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	yaml := "currency: EUR\nperSKUGPUHour:\n  NVIDIA-A100-SXM4-80GB: 4.20\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Currency != "EUR" {
		t.Fatalf("Currency = %q, want EUR", p.Currency)
	}
	if p.Source != path {
		t.Fatalf("Source = %q, want the file path %q so a reader knows this did not come from the built-in table", p.Source, path)
	}
	if got, ok := p.Rate("NVIDIA-A100-SXM4-80GB"); !ok || got != 4.20 {
		t.Fatalf("Rate(NVIDIA-A100-SXM4-80GB) = (%v, %v), want (4.20, true)", got, ok)
	}
}

// A file that declares its own `source` field is trusted over the path — the
// point of the field is to let a finance team say where their numbers came
// from, and the path is just where the file happened to sit.
func TestExplicitSourceFieldOverridesThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	yaml := "source: \"finance team Q3 quote\"\nperGPUHour: 3.0\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "finance team Q3 quote" {
		t.Fatalf("Source = %q, want the file's own declared source, not the path", p.Source)
	}
}

// Malformed YAML must error rather than silently yielding zero rates: a
// pricing file with a syntax error is a configuration mistake, and reporting
// it as "no pricing configured" would hide that mistake from the person who
// made it.
func TestMalformedPricingFileErrorsRatherThanYieldingZeroRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(path, []byte("not: [valid: yaml: at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pricing.Load(path); err == nil {
		t.Fatal("Load accepted syntactically invalid YAML with no error; the user's pricing file would silently price everything at zero")
	}
}

// A file whose top level is not a mapping (e.g. a bare scalar) is just as
// wrong as a syntax error, and must fail the same way.
func TestPricingFileThatIsNotAMappingErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scalar.yaml")
	if err := os.WriteFile(path, []byte("just a string\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pricing.Load(path); err == nil {
		t.Fatal("Load accepted a YAML scalar in place of the pricing mapping with no error")
	}
}

// A missing --pricing file must fail loudly. Falling back to the built-in
// table silently would mean a typo'd flag value quietly discarded a real
// negotiated rate and reported invented list prices in its place.
func TestMissingPricingFileErrorsRatherThanFallingBackSilently(t *testing.T) {
	dir := t.TempDir()
	if _, err := pricing.Load(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
		t.Fatal("Load accepted a nonexistent --pricing path with no error")
	}
}
