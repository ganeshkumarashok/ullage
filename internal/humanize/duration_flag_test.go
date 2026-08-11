package humanize_test

import (
	"flag"
	"testing"
	"time"

	"github.com/ullage-project/ullage/internal/humanize"
)

// DurationFlag is what actually sits behind --window on the command line, so
// its Set must reject exactly what ParseDuration rejects, and its String must
// round-trip into something a --help usage line or an error message can show.

func TestDurationFlagImplementsFlagValue(t *testing.T) {
	var _ flag.Value = &humanize.DurationFlag{}
}

func TestDurationFlagSetParsesTheSameGrammarAsParseDuration(t *testing.T) {
	var d humanize.DurationFlag
	if err := d.Set("14d"); err != nil {
		t.Fatalf("Set(14d) = %v", err)
	}
	if d.Value != 14*24*time.Hour {
		t.Fatalf("Value = %v, want 336h", d.Value)
	}
	if !d.Present {
		t.Fatal("Present = false after a successful Set; a caller cannot tell a real value from an unset default")
	}
}

// An invalid value must return an error and must not silently leave the flag
// looking like a deliberate zero: a caller who only checks Present would
// otherwise apply a zero-length scan window as if the user had asked for one.
func TestDurationFlagSetRejectsInvalidValueRatherThanLeavingAZero(t *testing.T) {
	var d humanize.DurationFlag
	err := d.Set("bogus")
	if err == nil {
		t.Fatal("Set(bogus) returned no error")
	}
	if d.Present {
		t.Fatal("Present = true after a failed Set; a rejected flag value must not look like a provided one")
	}
	if d.Value != 0 {
		t.Fatalf("Value = %v after a failed Set, want the zero value untouched", d.Value)
	}
}

func TestDurationFlagSetErrorDoesNotOverwriteAPreviousGoodValue(t *testing.T) {
	d := humanize.DurationFlag{}
	if err := d.Set("7d"); err != nil {
		t.Fatal(err)
	}
	if err := d.Set("bogus"); err == nil {
		t.Fatal("second Set(bogus) returned no error")
	}
	if d.Value != 7*24*time.Hour {
		t.Fatalf("Value = %v after a rejected second Set; the earlier good value must survive", d.Value)
	}
}

func TestDurationFlagStringRendersTheStoredValue(t *testing.T) {
	d := humanize.DurationFlag{}
	if s := d.String(); s != "" {
		t.Fatalf("String() on an unset flag = %q, want empty so usage text does not print a fake default", s)
	}
	if err := d.Set("36h"); err != nil {
		t.Fatal(err)
	}
	if s := d.String(); s != "1d 12h" {
		t.Fatalf("String() = %q, want the humanised form %q", s, "1d 12h")
	}
}

func TestDurationFlagStringOnNilReceiverDoesNotPanic(t *testing.T) {
	var d *humanize.DurationFlag
	if s := d.String(); s != "" {
		t.Fatalf("String() on a nil *DurationFlag = %q, want empty", s)
	}
}
