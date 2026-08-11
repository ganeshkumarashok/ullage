package api_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// The contract is the product for anyone who does not use the terminal — a
// k8sgpt analyzer, a Grafana backend, a CI gate, an operator. Those consumers
// do not compile against this package, so the Go type system protects nobody:
// renaming a JSON tag or dropping a field is invisible here and fatal there,
// and it surfaces as somebody else's broken dashboard weeks later.
//
// This test pins the wire shape. It fails on any rename, removal, retype, or
// addition. Additions are a deliberate false alarm: they are usually fine, but
// "usually fine" is the wrong default for a versioned contract, and updating
// the golden file is the moment to ask whether apiVersion should move.
func TestContractShapeIsPinned(t *testing.T) {
	got := strings.Join(shape(reflect.TypeOf(api.Result{}), ""), "\n") + "\n"
	golden := filepath.Join("testdata", "contract.txt")

	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("updated " + golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v\n\nrun: UPDATE_GOLDEN=1 go test ./pkg/ullage/api/", err)
	}
	if string(want) == got {
		return
	}

	for _, d := range diff(strings.Split(string(want), "\n"), strings.Split(got, "\n")) {
		t.Error(d)
	}
	t.Fatalf("the emitted contract no longer matches %s.\n\n"+
		"Every consumer of this tool reads these names. A rename or a removal is a breaking "+
		"change they will discover in production, so confirm the change is intended and whether "+
		"api.Version should move, then run:\n\n    UPDATE_GOLDEN=1 go test ./pkg/ullage/api/\n", golden)
}

// The version string is part of the contract too — consumers branch on it.
func TestVersionIsDeclared(t *testing.T) {
	if !strings.HasPrefix(api.Version, "ullage.dev/") {
		t.Fatalf("api.Version = %q, want a group-qualified version like ullage.dev/v0.1", api.Version)
	}
}

// shape renders a type as a sorted list of "json.path type" lines.
func shape(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Slice {
			path += "[]"
			ft = ft.Elem()
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
		}

		// time.Time and named scalars are leaves: they serialise as one value,
		// so descending into them would pin internals nobody can observe.
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			out = append(out, shape(ft, path)...)
			continue
		}
		out = append(out, path+" "+kind(ft))
	}
	sort.Strings(out)
	return out
}

func kind(t reflect.Type) string {
	switch {
	case t == reflect.TypeOf(time.Time{}):
		return "string(rfc3339)"
	case t == reflect.TypeOf(api.ISODuration(0)):
		return "string(iso8601-duration)"
	case t.Kind() == reflect.Map:
		return "object"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	}
	return t.Kind().String()
}

// diff reports set differences between two sorted line lists, which is all that
// is needed here and avoids a dependency for one test.
func diff(want, got []string) []string {
	in := func(xs []string, s string) bool {
		for _, x := range xs {
			if x == s {
				return true
			}
		}
		return false
	}
	var out []string
	for _, w := range want {
		if w != "" && !in(got, w) {
			out = append(out, "removed or renamed: "+w)
		}
	}
	for _, g := range got {
		if g != "" && !in(want, g) {
			out = append(out, "added or retyped:   "+g)
		}
	}
	return out
}
