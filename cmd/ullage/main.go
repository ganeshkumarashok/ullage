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
	"github.com/ullage-project/ullage/internal/config"
	"github.com/ullage-project/ullage/internal/demo"
	"github.com/ullage-project/ullage/internal/humanize"
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
  --output FORMAT        table | json (default table)
  --top N                rows to show (default 10)
  --namespace NS         restrict to a namespace (repeatable)
  --checks LIST          comma-separated check IDs to run
  --min-confidence LEVEL high | medium | low (default medium)
  --config PATH         suppression file (default .ullage.yaml)
  --explain-queries      print the PromQL used, then exit
  --no-cost              omit money from the output, and rank by
                         accelerator-hours instead of by spend

Tuning what counts:
  --idle-threshold DUR   minimum idle time before reporting (default 24h)
  --stuck-threshold DUR  minimum stuck time before reporting (default 1h)
  --step DUR             range query resolution (default 1h)

Connecting:
  --kubeconfig PATH      kubeconfig to use (default $KUBECONFIG, then ~/.kube/config)
  --context NAME         kubeconfig context to use
  --api-server URL       Kubernetes API server, if not the one in the kubeconfig
  --prometheus-auth MODE none | bearer | basic (default none)
  --prometheus-token TOKEN
  --prometheus-token-file PATH
                         file holding the bearer token, re-read at each scan
  --prometheus-username USER
  --prometheus-password PASS
  --prometheus-header K=V  extra request header (repeatable)
  --insecure-skip-tls-verify
                         do not verify the Prometheus certificate
  --timeout DUR          per-query timeout (default 60s)

Output:
  --pricing PATH         YAML of per-hour prices; overrides the built-in list
  --quiet                findings only, no header or footer
  --no-color             never colorize, even on a terminal
  --exit-zero            always exit 0, even when there are findings
  --trace                record the queries used in the JSON output

Environment:
  ULLAGE_PROMETHEUS      default for --prometheus

Exit codes:
  0  no findings above the threshold
  1  findings present
  2  the scan could not be completed
`

// durationValue accepts the units the help text advertises. Go's own duration
// flag stops at hours, so `--window 14d` — the default this tool prints — is a
// parse error, and the first command anyone copies out of the README fails.
type durationValue struct{ d *time.Duration }

func (v *durationValue) String() string {
	if v == nil || v.d == nil || *v.d == 0 {
		return ""
	}
	return humanize.Duration(*v.d)
}

func (v *durationValue) Set(s string) error {
	d, err := humanize.ParseDuration(s)
	if err != nil {
		return err
	}
	*v.d = d
	return nil
}

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

	// The dashed spellings are matched first: the subcommand switch below is
	// guarded on the argument not looking like a flag, so --version would
	// otherwise reach the flag parser and exit 2.
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-version", "-V":
			fmt.Printf("ullage %s\n", versionString())
			return nil
		case "--help", "-help":
			fmt.Print(usage)
			return nil
		}
	}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "scan":
			// `ullage` scans by default, but the usage text describes that as
			// "scan the current cluster", every doctor run ends with "ready to
			// scan", and the CronJob manifest shipped `args: [scan, ...]` for a
			// release before anyone ran it. When a tool has taught people a
			// verb, it should answer to it.
			return cmdScan(ctx, args[1:], nil)
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
		// --version is what people type. Reaching the flag parser it becomes
		// "flag provided but not defined" and exit 2, which is a poor first
		// impression and breaks the version probe in every install script.
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
	metricsSel    string
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
	config        string

	output   string
	top      int
	noCost   bool
	noColor  bool
	quiet    bool
	explainQ bool
	trace    bool
	exitZero bool
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
	f.fs.StringVar(&f.metricsSel, "metrics-selector", "",
		"PromQL label matcher naming this cluster, e.g. cluster=\"prod-eastus\" "+
			"(required when the endpoint holds more than one cluster)")
	f.fs.StringVar(&f.promAuth, "prometheus-auth", "", "none | bearer | basic")
	f.fs.StringVar(&f.promToken, "prometheus-token", "", "bearer token")
	f.fs.StringVar(&f.promTokenFile, "prometheus-token-file", "", "file containing a bearer token")
	f.fs.StringVar(&f.promUser, "prometheus-username", "", "basic auth username")
	f.fs.StringVar(&f.promPass, "prometheus-password", "", "basic auth password")
	f.fs.Var(&f.promHeader, "prometheus-header", "extra header, Key=Value (repeatable)")
	f.fs.BoolVar(&f.insecure, "insecure-skip-tls-verify", false, "skip TLS verification")
	f.timeout = 60 * time.Second
	f.fs.Var(&durationValue{&f.timeout}, "timeout", "per-query timeout")

	f.fs.Var(&durationValue{&f.window}, "window", "analysis window (default 14d)")
	f.fs.Var(&durationValue{&f.idle}, "idle-threshold", "minimum idle duration to report (default 24h)")
	f.fs.Var(&durationValue{&f.stuck}, "stuck-threshold", "minimum stuck duration to report (default 1h)")
	f.fs.Var(&durationValue{&f.step}, "step", "range query resolution (default 1h)")
	f.fs.StringVar(&f.minConfidence, "min-confidence", "", "high | medium | low (default medium)")
	f.fs.Var(&f.namespaces, "namespace", "restrict to a namespace (repeatable)")
	f.fs.StringVar(&f.checks, "checks", "", "comma-separated check IDs")
	f.fs.StringVar(&f.pricing, "pricing", "", "path to a pricing file")
	f.fs.StringVar(&f.config, "config", config.DefaultPath, "suppression file")

	f.fs.StringVar(&f.output, "output", "table", "table | json")
	f.fs.StringVar(&f.output, "o", "table", "table | json (shorthand)")
	f.fs.IntVar(&f.top, "top", 10, "rows to show")
	f.fs.BoolVar(&f.noCost, "no-cost", false, "omit cost estimates")
	f.fs.BoolVar(&f.noColor, "no-color", false, "disable colour")
	f.fs.BoolVar(&f.quiet, "quiet", false, "suppress progress output")
	f.fs.BoolVar(&f.explainQ, "explain-queries", false, "print the PromQL and exit")
	f.fs.BoolVar(&f.trace, "trace", false, "record queries in the result")
	f.fs.BoolVar(&f.exitZero, "exit-zero", false, "exit 0 even when findings exist")
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
	// --no-cost is honoured here rather than in the renderer. Stripping money
	// at the point it is displayed left the JSON output, the pricing block and
	// the per-row figures untouched, so a user who ran --no-cost before sharing
	// a report with finance published exactly the numbers they meant to remove.
	// Never loading the rates means no cost can be emitted by any surface.
	var prices *api.Pricing
	if !f.noCost {
		p, err := pricing.Load(f.pricing)
		if err != nil {
			return ullage.Options{}, err
		}
		prices = p
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
		Window:          f.window,
		IdleThreshold:   f.idle,
		StuckThreshold:  f.stuck,
		Step:            f.step,
		MinConfidence:   f.minConfidence,
		Namespaces:      f.namespaces,
		Checks:          checks,
		Pricing:         prices,
		ConfigFile:      f.config,
		MetricsSelector: f.metricsSel,
		Trace:           f.trace || f.explainQ,
		Version:         version,
	}, nil
}

func cmdScan(ctx context.Context, args []string, override func(*ullage.Options)) error {
	f := newFlags("ullage")
	if err := f.fs.Parse(args); err != nil {
		// `--help` is the first thing most people type. Letting it fall through
		// to the error path greeted them with an error-styled line and exit 2,
		// which breaks `ullage --help && ...` and any CI smoke test.
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usage)
			return nil
		}
		return fmt.Errorf("%v\n\n%s", err, usage)
	}
	opts, err := f.options()
	if err != nil {
		return err
	}
	if override != nil {
		override(&opts)
	}
	if err := validateWindow(f); err != nil {
		return err
	}
	if opts.Prometheus.URL == "" {
		if env := os.Getenv("ULLAGE_PROMETHEUS"); env != "" {
			opts.Prometheus.URL = env
		} else {
			return errors.New("--prometheus is required; try `ullage demo` to see the output shape first")
		}
	}

	// Progress uses a carriage return to rewrite one line in place, which only
	// works on a terminal. Piped or redirected — into a log, a file, or CI —
	// the control codes are not interpreted and every step concatenates into
	// one long unreadable line. A tool that garbles its own output the first
	// time someone pipes it has told them what to expect from the rest of it.
	progress := !f.quiet && f.output == "table" && isTerminal(os.Stderr)
	if progress {
		opts.Progress = func(msg string) { fmt.Fprintf(os.Stderr, "\r\033[K%s", msg) }
	}
	res, scanErr := ullage.Scan(ctx, opts)
	if progress {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	if scanErr != nil {
		return withDoctorHint(scanErr, opts.Prometheus.URL)
	}

	if f.explainQ {
		for _, q := range res.Scan.Queries {
			fmt.Println(q)
		}
		return nil
	}
	return emit(res, f)
}

// withDoctorHint attaches the one next step that actually resolves connection
// failures. `doctor` diagnoses these cases well -- it even spots a URL with
// /api/v1 on the end -- but the scan is the command people run first, and it
// used to hand back raw Go transport errors with nowhere to go from there.
func withDoctorHint(err error, promURL string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "404"):
		msg += "\n\n→ that endpoint answered, but not as Prometheus." +
			"\n  The URL should be the server root; it should not include /api/v1."
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "no series"):
	default:
		return err
	}
	if promURL != "" {
		msg += fmt.Sprintf("\n\n→ run `ullage doctor --prometheus %s` to check the prerequisites.", promURL)
	} else {
		msg += "\n\n→ run `ullage doctor` to check the prerequisites."
	}
	return errors.New(msg)
}

// validateWindow rejects the windows that cannot mean anything, rather than
// letting them reach Prometheus and come back as "the metrics endpoint returned
// no series" -- which is also what a missing dcgm-exporter looks like, so the
// user debugs the wrong thing.
func validateWindow(f *flags) error {
	if f.window < 0 {
		return fmt.Errorf("--window must be positive (e.g. 14d, 336h)")
	}
	if f.idle < 0 {
		return fmt.Errorf("--idle-threshold must be positive (e.g. 24h)")
	}
	if f.stuck < 0 {
		return fmt.Errorf("--stuck-threshold must be positive (e.g. 1h)")
	}
	if f.step < 0 {
		return fmt.Errorf("--step must be positive (e.g. 1h)")
	}
	if f.window > 0 && f.step > 0 && f.step > f.window {
		return fmt.Errorf("--step (%s) is longer than --window (%s), which would sample the window fewer than once",
			f.step, f.window)
	}
	if f.top < 0 {
		return fmt.Errorf("--top must be zero or more")
	}
	return nil
}

func emit(res *api.Result, f *flags) error {
	switch f.output {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	case "table":
		o := render.Options{
			Version: version, Top: f.top,
			MinConfidence: res.Scan.Params.MinConfidence,
			NoCost:        f.noCost,
			ConfigFile:    f.config,
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
	if n := len(res.Recommendations); n > 0 && !f.exitZero {
		return &findingsError{n: n}
	}
	return nil
}

// cmdDemo runs a full scan against an in-process fake cluster.
//
// This exists because the first question anyone asks about a tool like this is
// what its output looks like, and the honest answer should not require them to
// have a GPU cluster, a Prometheus and a set of credentials first.
// demoNow is the instant the demo cluster is anchored to.
//
// It floats with the wall clock so the demo reads as a live cluster rather than
// a museum piece. That makes its output change every hour, which is fine for a
// human and useless for a document: a transcript pasted into the README is
// stale within the hour, and there is no way to prove the README still matches
// the tool.
//
// ULLAGE_DEMO_NOW pins it. The docs test uses it to re-run the exact command
// printed in the README and diff the result, so the transcript is verified
// rather than trusted. It is deliberately an environment variable and not a
// flag: pinning time is a documentation-tooling concern, not a user-facing
// feature, and a --now flag would invite people to fake scan windows.
func demoNow() (time.Time, error) {
	raw, ok := os.LookupEnv("ULLAGE_DEMO_NOW")
	if !ok {
		return time.Now().UTC().Truncate(time.Hour), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("ULLAGE_DEMO_NOW=%q is not an RFC3339 instant: %w", raw, err)
	}
	return t.UTC(), nil
}

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

	now, err := demoNow()
	if err != nil {
		return err
	}
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

	return cmdScan(ctx, rest, func(o *ullage.Options) {
		o.APIServer = srv.Kube.URL
		o.Prometheus.URL = srv.Prometheus.URL
		o.Now = now
		// Without this the header carries the demo server's ephemeral loopback
		// port, so two runs of the same fixed fixture never produce the same
		// output and `ullage demo | diff` always reports a change.
		o.Context = "demo"
	})
}

func cmdExplain(ctx context.Context, args []string) error {
	id, rest := splitPositional(args)
	if id == "" {
		return errors.New("usage: ullage explain <finding-id>")
	}

	isDemo, filtered := stripDemo(rest)

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
		now, err := demoNow()
		if err != nil {
			return err
		}
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
	ok := true
	_, err = ullage.Doctor(ctx, opts, func(c ullage.DoctorCheck) {
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
	})
	if err != nil {
		return err
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

// splitPositional pulls the first bare word out of an argument list, wherever
// it appears, and returns everything else in order. `explain <id> --demo` and
// `explain --demo <id>` are both things people type, and the second used to
// take "--demo" as the finding id and then report that it did not exist.
//
// It stops at the first bare word rather than filtering all of them, so a value
// that follows a space-separated flag (`--config path explain-me`) is left
// attached to its flag.
func splitPositional(args []string) (string, []string) {
	rest := make([]string, 0, len(args))
	var id string
	for i, a := range args {
		if id == "" && !strings.HasPrefix(a, "-") && (i == 0 || !isFlagExpectingValue(args[i-1])) {
			id = a
			continue
		}
		rest = append(rest, a)
	}
	return id, rest
}

// isFlagExpectingValue reports whether a is a flag written without "=", and so
// consumes the next argument. Booleans never do.
func isFlagExpectingValue(a string) bool {
	if !strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
		return false
	}
	switch strings.TrimLeft(a, "-") {
	case "demo", "no-cost", "quiet", "no-color", "exit-zero", "explain-queries",
		"trace", "insecure-skip-tls-verify", "help", "h", "version":
		return false
	}
	return true
}

// stripDemo removes --demo, which is not a registered flag because it is a
// mode rather than a value.
func stripDemo(args []string) (bool, []string) {
	var on bool
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--demo" || a == "-demo" {
			on = true
			continue
		}
		rest = append(rest, a)
	}
	return on, rest
}

func cmdIgnore(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ullage ignore <finding-id> [--reason TEXT] [--until DATE]")
	}
	id := args[0]
	// Validated here rather than discovered later. A suppression whose id can
	// never match looks identical, in the file and in the output, to one that
	// works — the finding simply keeps appearing and the reader concludes the
	// feature is broken. Finding ids start with a check id, so that is
	// checkable at the moment of the mistake.
	if head, _, ok := strings.Cut(id, "/"); !ok {
		return fmt.Errorf("%q is not a finding id: ids look like <check>/<namespace>/<name>, "+
			"and the exact one is printed by `ullage explain`", id)
	} else if head != "*" {
		if _, known := check.Lookup(head); !known {
			return fmt.Errorf("%q does not start with a known check: got %q, want one of %s",
				id, head, strings.Join(checkIDs(), ", "))
		}
	}
	fs := flag.NewFlagSet("ignore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "why this is being suppressed")
	until := fs.String("until", "", "expiry date, YYYY-MM-DD")
	// Shares the flag name with the scan, so `--config` cannot mean two files.
	path := fs.String("config", config.DefaultPath, "suppression file to write to")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *reason == "" {
		return errors.New("--reason is required: a suppression without a reason is indistinguishable from a mistake six months later")
	}
	entry := fmt.Sprintf("  - id: %q\n    reason: %q\n", id, *reason)
	if *until != "" {
		if _, err := time.Parse("2006-01-02", *until); err != nil {
			return fmt.Errorf("--until must be YYYY-MM-DD: %w", err)
		}
		entry += fmt.Sprintf("    until: %q\n", *until)
	}

	existing, err := os.ReadFile(*path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out string
	if len(existing) == 0 {
		out = "# ullage suppressions\n" +
			"# Every entry needs a reason. An entry whose `until` has passed stops\n" +
			"# applying and is reported by the next scan so it can be removed; nothing\n" +
			"# here is ever rewritten automatically.\n" +
			"# An id may use * per path segment, e.g. idle-allocation/team-a/*\n" +
			"suppress:\n" + entry
	} else if strings.Contains(string(existing), "suppress:") {
		out = strings.TrimRight(string(existing), "\n") + "\n" + entry
	} else {
		out = strings.TrimRight(string(existing), "\n") + "\nsuppress:\n" + entry
	}
	if err := os.WriteFile(*path, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Printf("suppressed %s in %s\n", id, *path)
	return nil
}

// isTerminal reports whether a file is an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func checkIDs() []string {
	var out []string
	for _, c := range check.All() {
		out = append(out, c.Describe().ID)
	}
	return out
}
