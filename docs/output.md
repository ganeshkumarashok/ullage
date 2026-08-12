# Output

`ullage` prints a table by default. Two other formats are meant for passing the
result to someone else.

## HTML

```console
$ ullage demo --output html > report.html
```

One self-contained file. No network requests, no JavaScript needed to read it.
It opens with a capacity ledger that accounts for the whole window: what could
not be analysed, what is idle on purpose, what was flagged, and what is left.
You can check the headline figure against its parts.

Add `--redact` before sending it outside your team. Namespaces, workload names
and owner identities are replaced everywhere they appear, including inside
summaries, `kubectl` commands and link anchors. The grouping and the arithmetic
are untouched.

## JSON

```console
$ ullage demo --output json > result.json
```

A versioned document (`ullage.dev/v0.2`) defined in
[`pkg/ullage/api`](../pkg/ullage/api). It records the thresholds in effect, the
window, and the accelerator census, so two scans can be compared. Add `--trace`
to include every PromQL query the scan sent.

Top-level lists (`recommendations`, `byDesign`, `suppressed`, `notAnalyzed`,
`warnings`) always serialise as `[]`, never `null`. Optional lists nested inside
a finding use `omitempty` and may be absent, so read those defensively.

### The wire format is pinned

Consumers do not compile against this package, so renaming a JSON tag would pass
review here and break someone's dashboard later. A golden file,
[`pkg/ullage/api/testdata/contract.txt`](../pkg/ullage/api/testdata/contract.txt),
fails on any rename, removal, retype or addition. Additions fail on purpose:
updating the golden file is the right moment to ask whether `apiVersion` should
move.

```console
UPDATE_GOLDEN=1 go test ./pkg/ullage/api/
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Nothing found |
| 1 | Findings present |
| 2 | The scan could not complete |

That makes it usable as a CI gate. Use `--exit-zero` where a finding is not a
failure, such as a CronJob, where exit 1 would turn every successful scan into a
failed Job.

## As a library

```go
res, err := ullage.Scan(ctx, ullage.Options{
    Prometheus: ullage.PrometheusOptions{URL: promURL},
})
```

`Options.ConfigFile` is empty by default, so a library call will not read a
config file out of whatever directory the process started in.
