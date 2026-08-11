package render

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Options controls rendering.
type Options struct {
	Version       string
	Color         bool
	Width         int
	Top           int
	MinConfidence string
	NoCost        bool
}

// DetectTTY resolves colour and width from the environment.
//
// Colour is off when output is piped, when NO_COLOR is set, or when TERM is
// dumb. These are conventions, and a tool that ignores them produces escape
// codes in CI logs. Detection is stdlib-only: a character device that is not a
// pipe or a regular file is a terminal for our purposes, which avoids taking a
// dependency purely to ask one question.
func (o *Options) DetectTTY(w io.Writer) {
	o.Width = envWidth()
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || os.Getenv("TERM") == "" {
		o.Color = false
		return
	}
	f, ok := w.(*os.File)
	if !ok {
		o.Color = false
		return
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		o.Color = false
		return
	}
	o.Color = true
}

func envWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
	ansiDim   = "\033[2m"
)

type printer struct {
	w   io.Writer
	o   Options
	err error
}

func newPrinter(w io.Writer, o Options) *printer {
	if o.Width == 0 {
		o.Width = 80
	}
	return &printer{w: w, o: o}
}

func (p *printer) width() int {
	if p.o.Width < 60 {
		return 60
	}
	if p.o.Width > 100 {
		return 100
	}
	return p.o.Width
}

func (p *printer) line(format string, args ...any) {
	p.write(fmt.Sprintf(format, args...) + "\n")
}

func (p *printer) bold(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if p.o.Color {
		s = ansiBold + s + ansiReset
	}
	p.write(s + "\n")
}

func (p *printer) dim(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if p.o.Color {
		s = ansiDim + s + ansiReset
	}
	p.write(s + "\n")
}

func (p *printer) write(s string) {
	if p.err != nil {
		return
	}
	_, p.err = io.WriteString(p.w, s)
}

// wrapIndent wraps prose to the available width, continuing at a fixed indent.
func wrapIndent(s string, indent, width int) string {
	limit := width - indent
	if limit < 30 {
		limit = 30
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	pad := strings.Repeat(" ", indent)
	for i, word := range words {
		if lineLen > 0 && lineLen+1+len(word) > limit {
			b.WriteString("\n" + pad)
			lineLen = 0
		} else if i > 0 {
			b.WriteString(" ")
			lineLen++
		}
		b.WriteString(word)
		lineLen += len(word)
	}
	return b.String()
}
