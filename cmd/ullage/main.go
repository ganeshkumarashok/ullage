// Command ullage reports the accelerator capacity a Kubernetes cluster is
// paying for and not using, and what to do about each case.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ullage-project/ullage/internal/check"
	"github.com/ullage-project/ullage/internal/demo"
	"github.com/ullage-project/ullage/internal/pricing"
	"github.com/ullage-project/ullage/internal/render"
	"github.com/ullage-project/ullage/pkg/ullage"
	"github.com/ullage-project/ullage/pkg/ullage/api"
)

// Version is set at build time. The default is deliberately not "dev": a
// version string that lies is worse than one that admits it is a local build.
var (
	version = "v0.1.0-dev"
	commit  = ""
)

const usage = `ullage — the GPU your cluster paid for and didn't use.

Usage:
  ullage [flags]                  scan the current cluster
  ullage explain <finding-id>     show the full evidence for one finding
  ullage demo                     run against a built-in fake cluster
  ullage doctor                   check that the prerequisites are met
  ullage ignore <finding-id>      write a suppression to .ullage.yaml
  ullage checks                   list the available checks
  ullage version

Common flags:
  --prometheus URL       metrics endpoint (required unless --demo)
  --window DURATION      analysis window (default 14d)
  --output FORMAT        table | json | yaml (default table)
  --top N                rows to show (default 10)
  --namespace NS         restrict to a namespace (repeatable)
  --checks LIST          comma-separated check IDs to run
  --min-confidence LEVEL high | medium | low (default medium)
  --explain-queries      print the PromQL used, then exit
  --no-cost              omit money from the output

Exit codes:
  0  no findings above the threshold
  1  findings present
  2  the scan could not be completed
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		var fe *findingsError
		if errors.As(err, &fe) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "ullage: %v\n", err)
		os.Exit(2)
	}
}

// findingsError signals a successful scan that found something. It is an error
// only so that the exit code can be 1, which is what a CI gate needs.
type findingsError struct{ n int }

func (e *findingsError) Error() string { return fmt.Sprintf("%d findings", e.n) }

func run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "explain":
			return cmdExplain(ctx, args[1:])
		case "demo":
			return cmdDemo(ctx, args[1:])
		case "doctor":
			return cmdDoctor(ctx, args[1:])
		case "ignore":
			return cmdIgnore(args[1:])
		case "checks":
			return cmdChecks()
		case "version":
			fmt.Printf("ullage %s\n", versionString())
			return nil
		case "help":
			fmt.Print(usage)
			return nil
		default:
			return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
		}
	}
	return cmdScan(ctx, args, nil)
}

func versionString() string {
	if commit != "" {
		return version + " (" + commit + ")"
	}
	return version
}

type flags struct {
	fs *flag.FlagSet

	kubeconfig    string
	kubeContext   string
	apiServer     string
	promURL       string
	promAuth      string
	promToken     string
	promTokenFile string
	promUser      string
	promPass      string
	promHeader    stringList
	insecure      bool
	timeout       time.Duration

	window        time.Duration
	idle          time.Duration
	stuck         time.Duration
	step          time.Duration
	minConfidence string
	namespaces    stringList
	checks        string
	pricing       string

	output   string
	top      int
	noCost   bool
	noColor  bool
	quiet    bool
	explainQ bool
	trace    bool
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func newFlags(name string) *flags {
	f := &flags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	f.fs.SetOutput(io.Discard)

	f.fs.StringVar(&f.kubeconfig, "kubeconfig", "", "path to kubeconfig")
	f.fs.StringVar(&f.kubeContext, "context", "", "kubeconfig context")
	f.fs.StringVar(&f.apiServer, "api-server", "", "Kubernetes API URL, bypassing kubeconfig")
	f.fs.StringVar(&f.promURL, "prometheus", "", "Prometheus-compatible query endpoint")
	f.fs.StringVar(&f.promAuth, "prometheus-auth", "", "none | bearer | basic | azure-monitor")
	f.fs.StringVar(&f.promToken, "prometheus-token", "", "bearer token")
	f.fs.StringVar(&f.promTokenFile, "prometheus-token-file", "", "file containing a bearer token")
	f.fs.StringVar(&f.promUser, "prometheus-username", "", "basic auth username")
	f.fs.StringVar(&f.promPass, "prometheus-password", "", "basic auth password")
	f.fs.Var(&f.promHeader, "prometheus-header", "extra header, Key=Value (repeatable)")
	f.fs.BoolVar(&f.insecure, "insecure-skip-tls-verify", false, "skip TLS verification")
	f.fs.DurationVar(&f.timeout, "timeout", 60*time.Second, "per-query timeout")

	f.fs.DurationVar(&f.window, "window", 0, "analysis window (default 336h)")
	f.fs.DurationVar(&f.idle, "idle-threshold", 0, "minimum idle duration to report (default 72h)")
	f.fs.DurationVar(&f.stuck, "stuck-threshold", 0, "minimum stuck duration to report (default 1h)")
	f.fs.DurationVar(&f.step, "step", 0, "range query resolution (default 1h)")
	f.fs.StringVar(&f.minConfidence, "min-confidence", "", "high | medium | low (default medium)")
	f.fs.Var(&f.namespaces, "namespace", "restrict to a namespace (repeatable)")
	f.fs.StringVar(&f.checks, "checks", "", "comma-separated check IDs")
	f.fs.StringVar(&f.pricing, "pricing", "", "path to a pricing file")

	f.fs.StringVar(&f.output, "output", "table", "table | json | yaml")
	f.fs.StringVar(&f.output, "o", "table", "table | json | yaml (shorthand)")
	f.fs.IntVar(&f.top, "top", 10, "rows to show")
	f.fs.BoolVar(&f.noCost, "no-cost", false, "omit cost estimates")
	f.fs.BoolVar(&f.noColor, "no-color", false, "disable colour")
	f.fs.BoolVar(&f.quiet, "quiet", false, "suppress progress output")
	f.fs.BoolVar(&f.explainQ, "explain-queries", false, "print the PromQL and exit")
	f.fs.BoolVar(&f.trace, "trace", false, "record queries in the result")
	return f
}

func (f *flags) options() (ullage.Options, error) {
	headers := map[string]string{}
	for _, h := range f.promHeader {
		if k, v, ok := strings.Cut(h, "="); ok {
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	var checks []string
	if f.checks != "" {
		for _, c := range strings.Split(f.checks, ",") {
			if c = strings.TrimSpace(c); c != "" {
				checks = append(checks, c)
			}
		}
	}
	prices, err := pricing.Load(f.pricing)
	if err != nil {
		return ullage.Options{}, err
	}

	return ullage.Options{
		Kubeconfig: f.kubeconfig,
		Context:    f.kubeContext,
		APIServer:  f.apiServer,
		Prometheus: ullage.PrometheusOptions{
			URL: f.promURL, Auth: f.promAuth, Token: f.promToken,
			TokenFile: f.promTokenFile, Username: f.promUser, Password: f.promPass,
			Headers: headers, Timeout: f.timeout, Insecure: f.insecure,
		},
		Window:         f.window,
		IdleThreshold:  f.idle,
		StuckThreshold: f.stuck,
		Step:           f.step,
		MinConfidence:  f.minConfidence,
		Namespaces:     f.namespaces,
		Checks:         checks,
		Pricing:        prices,
		Trace:          f.trace || f.explainQ,
		Version:        version,
	}, nil
}

func cmdScan(ctx context.Context, args []string, override func(*ullage.Options)) error {
	f := newFlags("ullage")
	if err := f.fs.Parse(args); err != nil {
		return fmt.Errorf("%v\n\n%s", err, usage)
	}
	opts, err := f.options()
	if err != nil {
		return err
	}
	if override != nil {
		override(&opts)
	}
	if opts.Prometheus.URL == "" {
		if env := os.Getenv("ULLAGE_PROMETHEUS"); env != "" {
			opts.Prometheus.URL = env
		} else {
			return errors.New("--prometheus is required; try `ullage demo` to see the output shape first")
		}
	}

	if !f.quiet && f.output == "table" {
		opts.Progress = func(msg string) { fmt.Fprintf(os.Stderr, "\r\033[K%s", msg) }
	}
	res, scanErr := ullage.Scan(ctx, opts)
	if !f.quiet && f.output == "table" {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if scanErr != nil {
		return scanErr
	}

	if f.explainQ {
		for _, q := range res.Scan.Queries {
			fmt.Println(q)
		}
		return nil
	}
	return emit(res, f)
}

func emit(res *api.Result, f *flags) error {
	switch f.output {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	case "yaml":
		return errors.New("yaml output is not implemented in v0.1; use --output json")
	case "table":
		o := render.Options{
			Version: version, Top: f.top,
			MinConfidence: res.Scan.Params.MinConfidence,
			NoCost:        f.noCost,
		}
		o.DetectTTY(os.Stdout)
		if f.noColor {
			o.Color = false
		}
		if err := render.Table(os.Stdout, res, o); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown output format %q", f.output)
	}
	if n := len(res.Recommendations); n > 0 {
		return &findingsError{n: n}
	}
	return nil
}

// cmdDemo runs a full scan against an in-process fake cluster.
//
// This exists because the first question anyone asks about a tool like this is
// what its output looks like, and the honest answer should not require them to
// have a GPU cluster, a Prometheus and a set of credentials first.
func cmdDemo(ctx context.Context, args []string) error {
	var serve bool
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--serve" || a == "-serve" {
			serve = true
			continue
		}
		rest = append(rest, a)
	}

	now := time.Now().UTC().Truncate(time.Hour)
	srv := demo.Start(now)
	defer srv.Close()

	if serve {
		fmt.Printf("demo cluster running\n\n")
		fmt.Printf("  kube API    %s\n", srv.Kube.URL)
		fmt.Printf("  prometheus  %s\n\n", srv.Prometheus.URL)
		fmt.Printf("scan it with:\n\n")
		fmt.Printf("  ullage --api-server %s --prometheus %s\n\n", srv.Kube.URL, srv.Prometheus.URL)
		fmt.Println("press ctrl-c to stop")
		<-ctx.Done()
		return nil
	}

	fmt.Fprintln(os.Stderr, "running against a built-in demo cluster — no real resources are touched")
	fmt.Fprintln(os.Stderr, "")

	return cmdScan(ctx, rest, func(o *ullage.Options) {
		o.APIServer = srv.Kube.URL
		o.Prometheus.URL = srv.Prometheus.URL
		o.Now = now
	})
}

func cmdExplain(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ullage explain <finding-id>")
	}
	id := args[0]
	rest := args[1:]

	var isDemo bool
	filtered := make([]string, 0, len(rest))
	for _, a := range rest {
		if a == "--demo" || a == "-demo" {
			isDemo = true
			continue
		}
		filtered = append(filtered, a)
	}

	f := newFlags("ullage explain")
	if err := f.fs.Parse(filtered); err != nil {
		return err
	}
	opts, err := f.options()
	if err != nil {
		return err
	}
	opts.Trace = true

	var srv *demo.Servers
	if isDemo {
		now := time.Now().UTC().Truncate(time.Hour)
		srv = demo.Start(now)
		defer srv.Close()
		opts.APIServer = srv.Kube.URL
		opts.Prometheus.URL = srv.Prometheus.URL
		opts.Now = now
	}
	if opts.Prometheus.URL == "" {
		if env := os.Getenv("ULLAGE_PROMETHEUS"); env != "" {
			opts.Prometheus.URL = env
		} else {
			return errors.New("--prometheus is required")
		}
	}

	res, err := ullage.Scan(ctx, opts)
	if err != nil {
		return err
	}
	all := append(append([]api.Finding{}, res.Recommendations...), res.ByDesign...)
	all = append(all, res.Suppressed...)

	// An identifier is accepted in whichever form the reader has in hand: the
	// full check-qualified id from JSON, or the workload reference printed in
	// the table. Making someone retype a prefix they were never shown is a
	// small cruelty that costs a tool its second use.
	var matches []*api.Finding
	for i := range all {
		if all[i].ID == id || all[i].Workload.Ref() == id ||
			strings.HasSuffix(all[i].ID, "/"+id) {
			matches = append(matches, &all[i])
		}
	}
	switch len(matches) {
	case 1:
		o := render.Options{Version: version}
		o.DetectTTY(os.Stdout)
		return render.Explain(os.Stdout, matches[0], res, o)
	case 0:
		return fmt.Errorf("no finding matching %q in this scan\n\nthe cluster may have changed since the id was produced; run `ullage` again for current ids", id)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q matches %d findings:\n", id, len(matches))
		for _, m := range matches {
			fmt.Fprintf(&b, "  %s\n", m.ID)
		}
		return errors.New(b.String())
	}
}

// cmdDoctor answers "will this work here" without running an analysis, because
// a first run that fails should say which prerequisite is missing rather than
// return an empty table.
func cmdDoctor(ctx context.Context, args []string) error {
	f := newFlags("ullage doctor")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	opts, err := f.options()
	if err != nil {
		return err
	}
	if opts.Prometheus.URL == "" {
		opts.Prometheus.URL = os.Getenv("ULLAGE_PROMETHEUS")
	}
	report, err := ullage.Doctor(ctx, opts)
	if err != nil {
		return err
	}
	ok := true
	for _, c := range report.Checks {
		mark := "ok  "
		switch c.Status {
		case "fail":
			mark, ok = "FAIL", false
		case "warn":
			mark = "warn"
		}
		fmt.Printf("  [%s] %s\n", mark, c.Name)
		if c.Detail != "" {
			fmt.Printf("         %s\n", c.Detail)
		}
		if c.Remedy != "" {
			fmt.Printf("         → %s\n", c.Remedy)
		}
	}
	fmt.Println()
	if !ok {
		return errors.New("prerequisites are not met")
	}
	fmt.Println("ready to scan.")
	return nil
}

func cmdChecks() error {
	fmt.Println("available checks:")
	fmt.Println()
	for _, c := range check.All() {
		d := c.Describe()
		fmt.Printf("  %-14s %s\n", d.ID, d.Title)
		fmt.Printf("  %-14s claim: %s\n", "", d.Claim)
		if d.Risk != "" {
			fmt.Printf("  %-14s risk:  %s\n", "", d.Risk)
		}
		fmt.Println()
	}
	return nil
}

func cmdIgnore(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ullage ignore <finding-id> [--reason TEXT] [--until DATE]")
	}
	id := args[0]
	fs := flag.NewFlagSet("ignore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "why this is being suppressed")
	until := fs.String("until", "", "expiry date, YYYY-MM-DD")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *reason == "" {
		return errors.New("--reason is required: a suppression without a reason is indistinguishable from a mistake six months later")
	}
	path := ".ullage.yaml"
	entry := fmt.Sprintf("  - id: %q\n    reason: %q\n", id, *reason)
	if *until != "" {
		if _, err := time.Parse("2006-01-02", *until); err != nil {
			return fmt.Errorf("--until must be YYYY-MM-DD: %w", err)
		}
		entry += fmt.Sprintf("    until: %q\n", *until)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out string
	if len(existing) == 0 {
		out = "# ullage suppressions\n# every entry needs a reason, and entries with an expiry are removed\n# automatically once it passes.\nsuppress:\n" + entry
	} else if strings.Contains(string(existing), "suppress:") {
		out = strings.TrimRight(string(existing), "\n") + "\n" + entry
	} else {
		out = strings.TrimRight(string(existing), "\n") + "\nsuppress:\n" + entry
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Printf("suppressed %s in %s\n", id, path)
	return nil
}
