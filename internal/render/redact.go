package render

import (
	"html/template"
	"reflect"
	"regexp"
	"strings"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// A redactor rewrites every identifier a scan collected out of the document.
//
// The obvious way to build this feature is to call a redact function on each
// field that looks sensitive. That approach fails open: it protects the fields
// someone remembered on the day, and silently leaks the next one anybody adds.
// It also misses names embedded inside prose that was assembled upstream — a
// finding summary, a kubectl command, a rationale — which is where most of them
// actually are.
//
// So this works the other way round. It collects the identifiers from the
// result itself, then rewrites every string in the rendered document. A field
// added later is covered without anyone remembering to think about it, and the
// test can make the strong assertion: no identifier that went in comes out.
type redactor struct {
	// names maps an identifier to its mask. Lookup is by whole token rather
	// than by substring: a namespace called "gpu" must not turn every mention
	// of the word into a row of dots.
	names map[string]string
}

// token is the shape of a Kubernetes name, a node name or a user identity.
// Splitting on it means a replacement can only ever consume a complete
// identifier, which removes both the substring-collision problem and the
// ordering that would otherwise be needed to avoid it.
var token = regexp.MustCompile(`[A-Za-z0-9_.@-]+`)

// newRedactor harvests the identifiers out of a result.
func newRedactor(res *api.Result) *redactor {
	r := &redactor{names: map[string]string{}}

	add := func(s string) {
		// A single character is not identifying. A value holding a separator
		// is a compound reference whose parts are added individually, and
		// would never match a token anyway.
		if len(s) < 2 || strings.ContainsAny(s, "/ ") {
			return
		}
		r.names[s] = mask(s)
	}

	addRef := func(s string) {
		// A "ns/name" reference also appears with its halves apart.
		for _, part := range strings.Split(s, "/") {
			add(part)
		}
	}

	add(res.Scan.Context)
	// A URL is punctuation-heavy, so harvest the pieces a token can match:
	// the host and the port, wherever else they turn up.
	for _, t := range token.FindAllString(res.Scan.PrometheusURL, -1) {
		add(t)
	}

	// Every list, not just the recommendations: a name suppressed in one
	// section is still a name, and it appears verbatim in the others.
	for _, list := range [][]api.Finding{res.Recommendations, res.ByDesign, res.Suppressed} {
		for _, f := range list {
			addRef(f.Workload.Ref())
			add(f.Workload.Namespace)
			add(f.Workload.Name)
			for _, m := range f.Workload.Members {
				addRef(m)
			}
			add(f.Owner.Identity)
			add(f.Provenance.RootName)
			for _, c := range f.Provenance.Chain {
				add(c.Name)
			}
			addRef(f.Fix.Targets)
		}
	}
	// Exclusions carry no names of their own: Detail describes a class of
	// device, not an instance. Nothing to harvest, and nothing is lost.

	return r
}

// apply rewrites one string, one whole token at a time.
func (r *redactor) apply(s string) string {
	if r == nil || s == "" {
		return s
	}
	return token.ReplaceAllStringFunc(s, func(t string) string {
		if m, ok := r.names[t]; ok {
			return m
		}
		return t
	})
}

// scrub walks a value and rewrites every string it reaches. The view model is
// plain data — structs, slices, maps, strings — so a walk over it is total.
func (r *redactor) scrub(v reflect.Value) {
	if r == nil || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		// The stylesheet and the script are compiled into the binary and hold
		// no cluster data. Rewriting them would corrupt the document, and
		// their types are exactly what marks them as ours rather than the
		// cluster's.
		switch v.Interface().(type) {
		case template.CSS, template.JS, template.HTML, template.URL, template.Srcset, template.JSStr:
			return
		}
		if v.CanSet() {
			v.SetString(r.apply(v.String()))
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			r.scrub(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			r.scrub(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			r.scrub(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			// Map values are not addressable, so rebuild each entry.
			val := reflect.New(v.Type().Elem()).Elem()
			val.Set(v.MapIndex(k))
			r.scrub(val)
			v.SetMapIndex(k, val)
		}
	}
}

// mask keeps a name discussable without publishing it. The first character and
// the length class survive, which is enough for a reader to tell two redacted
// workloads apart while they talk about the report.
func mask(s string) string {
	r := []rune(s)
	if len(r) <= 2 {
		return strings.Repeat("•", len(r))
	}
	n := len(r) - 1
	if n > 7 {
		n = 7
	}
	return string(r[0]) + strings.Repeat("•", n)
}
