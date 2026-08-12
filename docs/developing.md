# Developing

`ullage` depends on one third-party Go module,
[`yaml.v3`](https://gopkg.in/yaml.v3). There is no client-go, no
controller-runtime and no vendored Kubernetes tree; it talks to the API server
over ordinary HTTP(S) with the types it needs. A clone builds in a few seconds
and the tests run without a cluster.

```console
make check        # fmt, vet, and the full race-enabled test suite
make cover        # tests with a coverage profile, then open the HTML report
make demo         # the transcript in the README
make tour         # the narrated walkthrough
```

`make check` also runs the example scripts, so a change that breaks one is
caught before it ships as documented behaviour.

## Adding a check

A check is one file. It reads a normalised fact layer, with no Kubernetes types
and no Prometheus types, and returns what it saw. Ownership, provenance, the fix
command, grouping, ranking, pricing and rendering happen downstream, so a new
check gets all of that for free.

```go
type MyCheck struct{}

func (MyCheck) Describe() check.Descriptor        { ... }
func (MyCheck) Applicable(d inventory.Device) bool { ... }
func (MyCheck) Run(ctx context.Context, cl *inventory.Cluster, p check.Params) ([]check.RawFinding, error)

func init() { check.Register(MyCheck{}) }
```

`Describe` requires you to state what the check claims and the risk of acting on
it. If you cannot name a way the check could be wrong, it is not ready.

Because checks read facts, their tests are literals. No server, no fixtures. See
[`internal/check/check_test.go`](../internal/check/check_test.go).

## Testing against a real cluster

The tests above run against in-memory fakes. `e2e/kind.sh` runs the same code
against a real Kubernetes cluster: three [kind](https://kind.sigs.k8s.io) nodes
with fake accelerator capacity, a real Prometheus, and a synthetic exporter
reporting one busy pod and one idle one.

```console
make e2e-kind          # create, deploy, scan, assert; ~3 minutes
./e2e/kind.sh scan     # re-run just the scan against a cluster already up
./e2e/kind.sh rbac     # run ullage in-cluster with only deploy/rbac.yaml
make e2e-kind-down     # delete it
```

It asserts on behaviour rather than on printed text: the idle pod must be
reported, the busy pod must not be, the accelerator census must reconcile, and
the idle finding must be attributed to exactly one device.

`./e2e/kind.sh rbac` is the one worth knowing about. It builds the image,
applies [`deploy/rbac.yaml`](../deploy/rbac.yaml), and scans from inside the
cluster as that ServiceAccount and nothing more. Developer kubeconfigs are
usually cluster-admin, so a check that starts reading a new resource passes
every other test and then fails only for the people who installed the published
manifest.

The script waits for real sample coverage instead of sleeping, so it behaves the
same on a slow machine. It needs `kind`, `kubectl`, `docker` and `python3`.
