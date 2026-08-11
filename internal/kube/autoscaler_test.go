package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// caServer serves the cluster-autoscaler status ConfigMap with the given data.
func caServer(t *testing.T, data map[string]string, code int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "cluster-autoscaler-status") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if code != 0 {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "code": code})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIServer: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The autoscaler's minimum size is the difference between "this pool is empty
// and wasting money" and "this pool is empty because you told it to be". Recent
// autoscalers publish structured YAML under the `status` key.
func TestClusterAutoscalerStatusParsesStructuredYAML(t *testing.T) {
	c := caServer(t, map[string]string{"status": `
nodeGroups:
  - name: aks-h100reserve-1234-vmss
    minSize: 2
    maxSize: 8
    health:
      nodeCounts:
        registered:
          total: 2
          ready: 2
  - name: aks-burst-5678-vmss
    minSize: 0
    maxSize: 20
    health:
      nodeCounts:
        registered:
          total: 3
          ready: 3
`}, 0)

	st, err := c.ClusterAutoscalerStatus(context.Background())
	if err != nil {
		t.Fatalf("ClusterAutoscalerStatus: %v", err)
	}
	if st == nil {
		t.Fatal("no status parsed, so a deliberately reserved pool is indistinguishable " +
			"from a wasted one and ullage will offer to delete nodes an operator pinned")
	}
	g, ok := st.Groups["aks-h100reserve-1234-vmss"]
	if !ok {
		t.Fatalf("reserved group missing: %+v", st.Groups)
	}
	if g.MinSize != 2 {
		t.Errorf("minSize = %d, want 2: an unread floor is a floor ullage will recommend "+
			"scaling below", g.MinSize)
	}
	if g.MaxSize != 8 || g.Ready != 2 {
		t.Errorf("maxSize = %d ready = %d, want 8 and 2", g.MaxSize, g.Ready)
	}
	if b := st.Groups["aks-burst-5678-vmss"]; b.MinSize != 0 || b.MaxSize != 20 {
		t.Errorf("burst group = %+v, want min 0 max 20", b)
	}
}

// Some autoscaler builds report the sizes only under health. Reading the outer
// field alone silently yields a floor of zero, which reads as "nothing is
// reserved" -- the permissive direction, and the one that produces a command
// against a pool somebody pinned.
func TestClusterAutoscalerStatusFallsBackToHealthSizes(t *testing.T) {
	c := caServer(t, map[string]string{"status": `
nodeGroups:
  - name: reserved
    health:
      minSize: 4
      maxSize: 12
      nodeCounts:
        registered:
          ready: 4
`}, 0)

	st, err := c.ClusterAutoscalerStatus(context.Background())
	if err != nil || st == nil {
		t.Fatalf("status = %v, err = %v", st, err)
	}
	if g := st.Groups["reserved"]; g.MinSize != 4 || g.MaxSize != 12 {
		t.Fatalf("group = %+v, want min 4 max 12: the sizes were published only under "+
			"health, and reading zero there means a pinned pool looks free to delete", g)
	}
}

// Older autoscalers publish a free-text blob. The floor is worth having even
// from a format nobody would choose to parse.
func TestClusterAutoscalerStatusParsesLegacyText(t *testing.T) {
	c := caServer(t, map[string]string{"status": `Cluster-autoscaler status at 2026-08-11:
Cluster-wide:
  Health:      Healthy (ready=6 unready=0 registered=6)

NodeGroups:
  Name:        aks-h100reserve-1234-vmss
  Health:      Healthy (ready=2 unready=0 registered=2 cloudProviderTarget=2 minSize=2 maxSize=8)

  Name:        aks-burst-5678-vmss
  Health:      Healthy (ready=4 unready=0 registered=4 cloudProviderTarget=4 minSize=0 maxSize=20)
`}, 0)

	st, err := c.ClusterAutoscalerStatus(context.Background())
	if err != nil || st == nil {
		t.Fatalf("status = %v, err = %v", st, err)
	}
	g, ok := st.Groups["aks-h100reserve-1234-vmss"]
	if !ok {
		t.Fatalf("legacy group missing: %+v", st.Groups)
	}
	if g.MinSize != 2 || g.MaxSize != 8 || g.Ready != 2 {
		t.Fatalf("group = %+v, want min 2 max 8 ready 2", g)
	}
	if b := st.Groups["aks-burst-5678-vmss"]; b.MinSize != 0 || b.MaxSize != 20 {
		t.Errorf("second group = %+v, want min 0 max 20: parsing must not stop after the "+
			"first entry, or every pool but one loses its floor", b)
	}
}

// Most clusters do not run the autoscaler, and many service accounts cannot
// read kube-system. Neither is an error, but neither may be reported as "no
// floor exists" either -- the caller distinguishes them, and that only works if
// this returns nil rather than an empty status.
func TestClusterAutoscalerAbsenceIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		data map[string]string
	}{
		{"absent", http.StatusNotFound, nil},
		{"forbidden", http.StatusForbidden, nil},
		{"present but empty", 0, map[string]string{"status": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := caServer(t, tc.data, tc.code).ClusterAutoscalerStatus(context.Background())
			if err != nil {
				t.Fatalf("err = %v: this is a normal cluster state, not a failure", err)
			}
			if st != nil {
				t.Fatalf("status = %+v, want nil: an empty status reads as a confirmed "+
					"absence of any floor, which is a claim that was never checked", st)
			}
		})
	}
}

// Karpenter disrupts for Underutilized, Empty and Drifted. Only the first two
// are how an idle GPU node goes away. A budget scoped to Drifted pins node
// replacement during an AMI rollout and says nothing about consolidating an
// empty node -- so treating it as a hold silently suppresses a real finding on
// exactly the clusters careful enough to scope their budgets.
func TestKarpenterBudgetReasonsAreHonoured(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []string
		want    bool
	}{
		{"no reasons means every reason", nil, true},
		{"underutilized is how an idle node goes", []string{"Underutilized"}, true},
		{"empty is how an empty node goes", []string{"Empty"}, true},
		{"mixed list containing a relevant reason", []string{"Drifted", "Empty"}, true},
		{"case is not significant", []string{"underutilized"}, true},
		{"drift alone does not hold an idle node", []string{"Drifted"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetBlocksReclaim(tc.reasons); got != tc.want {
				t.Errorf("budgetBlocksReclaim(%v) = %v, want %v", tc.reasons, got, tc.want)
			}
		})
	}
}

func TestZeroNodeBudget(t *testing.T) {
	for in, want := range map[string]bool{
		"0": true, "0%": true, " 0 ": true,
		"1": false, "10%": false, "": false, "0.5": false,
	} {
		if got := zeroNodeBudget(in); got != want {
			t.Errorf("zeroNodeBudget(%q) = %v, want %v", in, got, want)
		}
	}
}
