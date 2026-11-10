package certrestart

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/checktest"
	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/kube"
	"github.com/thyarles/lhc/internal/runner"
)

var now0 = time.Date(2026, 11, 10, 3, 20, 0, 0, time.UTC)

const (
	adminCrt   = "/var/lib/rancher/rke2/server/tls/client-admin.crt"
	kubectl    = "/var/lib/rancher/rke2/bin/kubectl"
	kubeconfig = "/etc/rancher/rke2/rke2.yaml"
)

// node is a fake rke2 node: `systemctl restart` is recorded and runs
// onRestart, which is where a test renews the certificates.
type node struct {
	*checktest.Runner
	mu        sync.Mutex
	restarts  []string
	onRestart func()
}

func (n *node) Run(ctx context.Context, name string, args ...string) runner.Result {
	if name == "systemctl" && len(args) == 2 && args[0] == "restart" {
		n.mu.Lock()
		n.restarts = append(n.restarts, args[1])
		n.mu.Unlock()
		if n.onRestart != nil {
			n.onRestart()
		}
		return runner.Result{}
	}
	return n.Runner.Run(ctx, name, args...)
}

type harness struct {
	t      *testing.T
	n      *node
	clock  time.Time
	log    []string
	claims []hub.Claim
	grants int // how many of the three collectors grant
}

func newHarness(t *testing.T, expiresIn time.Duration) *harness {
	t.Helper()
	old := kube.Getenv
	kube.Getenv = func(string) string { return "" }
	t.Cleanup(func() { kube.Getenv = old })
	h := &harness{t: t, n: &node{Runner: checktest.NewRunner()}, clock: now0, grants: 3}
	if expiresIn != 0 {
		h.n.File(adminCrt, checktest.CertPEM(t, "system:admin", now0.Add(expiresIn), false))
	}
	h.n.Expect("systemctl is-active rke2-server", "active")
	h.n.onRestart = func() {
		h.n.File(adminCrt, checktest.CertPEM(t, "system:admin", now0.Add(365*24*time.Hour), false))
	}
	return h
}

func (h *harness) withKubeconfig(nodes string) {
	h.n.Tool(kubectl)
	h.n.File(kubeconfig, "apiVersion: v1\n")
	h.n.Expect("get nodes -o jsonpath", nodes)
}

func (h *harness) run(dryRun bool) Result {
	h.t.Helper()
	d := Deps{
		Runner: h.n,
		Now:    func() time.Time { return h.clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			h.clock = h.clock.Add(d)
			return nil
		},
		Ask: func(_ context.Context, c hub.Claim) []hub.Reply {
			h.claims = append(h.claims, c)
			out := make([]hub.Reply, 3)
			for i := range out {
				out[i] = hub.Reply{Collector: fmt.Sprintf("http://c%d.example.com:8461", i+1), Answer: hub.Answer{Granted: i < h.grants, Reason: "k8s-node02.example.com restarts today"}}
			}
			return out
		},
		Collectors: 3,
		NewID:      func() (string, error) { return "claim-id", nil },
		Log:        func(f string, a ...any) { h.log = append(h.log, fmt.Sprintf(f, a...)) },
	}
	res, err := Run(context.Background(), Options{Host: "k8s-node01.example.com", Group: "k8s-node", Before: 30 * 24 * time.Hour, DryRun: dryRun}, d)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *harness) logged(sub string) bool {
	for _, l := range h.log {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (h *harness) noRestartNoClaim(t *testing.T) {
	t.Helper()
	if len(h.n.restarts) != 0 || len(h.claims) != 0 {
		t.Fatalf("restarts %v claims %v; log %q", h.n.restarts, h.claims, h.log)
	}
}

func TestNoCertificatesNothingToDo(t *testing.T) {
	h := newHarness(t, 0)
	if r := h.run(false); r.Outcome != NoCerts {
		t.Fatal(r.Outcome)
	}
	h.noRestartNoClaim(t)
}

func TestNotDueSendsNoClaim(t *testing.T) {
	h := newHarness(t, 100*24*time.Hour)
	if r := h.run(false); r.Outcome != NotDue {
		t.Fatal(r.Outcome)
	}
	h.noRestartNoClaim(t)
	if !h.logged("not due: first certificate (client-admin.crt) expires in 100d, restart at 30d") {
		t.Fatalf("%q", h.log)
	}
}

func TestAStoppedServiceIsLeftAlone(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.n.Runner = checktest.NewRunner() // nothing active
	h.n.File(adminCrt, checktest.CertPEM(t, "system:admin", now0.Add(20*24*time.Hour), false))
	if r := h.run(false); r.Outcome != NoService {
		t.Fatal(r.Outcome)
	}
	h.noRestartNoClaim(t)
}

func TestANotReadyNodeStopsTheNight(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.withKubeconfig("k8s-node01\tTrue\nk8s-node02\tUnknown\n")
	if r := h.run(false); r.Outcome != Unhealthy {
		t.Fatal(r.Outcome)
	}
	h.noRestartNoClaim(t)
	if !h.logged("not every node is Ready (k8s-node02)") {
		t.Fatalf("%q", h.log)
	}
}

func TestAClusterThatCannotBeAskedStopsTheNight(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.n.Tool(kubectl)
	h.n.File(kubeconfig, "apiVersion: v1\n")
	h.n.Expect("get nodes -o jsonpath", "").Code(1).Stderr("The connection to the server 127.0.0.1:6443 was refused")
	if r := h.run(false); r.Outcome != Unhealthy {
		t.Fatal(r.Outcome)
	}
	h.noRestartNoClaim(t)
}

func TestNoMajorityNoRestart(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.grants = 1
	if r := h.run(false); r.Outcome != Refused {
		t.Fatal(r.Outcome)
	}
	if len(h.n.restarts) != 0 || len(h.claims) != 1 {
		t.Fatalf("restarts %v claims %d", h.n.restarts, len(h.claims))
	}
	if !h.logged("refused: k8s-node02.example.com restarts today") || !h.logged("1 of 3 collectors granted, 2 needed") {
		t.Fatalf("%q", h.log)
	}
}

func TestAMajorityRestartsAndRenews(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.withKubeconfig("k8s-node01\tTrue\nk8s-node02\tTrue\n")
	h.grants = 2
	r := h.run(false)
	if r.Outcome != Restarted || !r.Renewed || r.Service != "rke2-server" || !r.At.Equal(now0) {
		t.Fatalf("%+v", r)
	}
	if len(h.n.restarts) != 1 || h.n.restarts[0] != "rke2-server" {
		t.Fatalf("restarts %v", h.n.restarts)
	}
	c := h.claims[0]
	if c.Kind != hub.KindCertRestart || c.Host != "k8s-node01.example.com" || c.Group != "k8s-node" || c.ID != "claim-id" || c.Probe {
		t.Fatalf("claim %+v", c)
	}
	if !h.logged("renewed: first certificate now expires in 365d") || !h.logged("active again and every node is Ready") {
		t.Fatalf("%q", h.log)
	}
}

func TestARestartThatDoesNotRenewIsSaid(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.n.onRestart = nil
	r := h.run(false)
	if r.Outcome != Restarted || r.Renewed {
		t.Fatalf("%+v", r)
	}
	if !h.logged("restart did not renew the certificates (first still expires in 20d)") {
		t.Fatalf("%q", h.log)
	}
}

func TestAServiceThatDoesNotComeBackIsSaid(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	h.n.Runner = checktest.NewRunner()
	h.n.File(adminCrt, checktest.CertPEM(t, "system:admin", now0.Add(20*24*time.Hour), false))
	h.n.Expect("systemctl is-active rke2-server", "active").Once()
	h.n.Expect("systemctl is-active rke2-server", "failed")
	r := h.run(false)
	if r.Outcome != Restarted || !h.logged("rke2-server is not active 10m0s after the restart") {
		t.Fatalf("%+v %q", r, h.log)
	}
	if h.clock.Sub(now0) < backTimeout {
		t.Fatalf("gave up after %s", h.clock.Sub(now0))
	}
}

func TestADryRunProbesAndRestartsNothing(t *testing.T) {
	h := newHarness(t, 20*24*time.Hour)
	if r := h.run(true); r.Outcome != WouldDo {
		t.Fatal(r.Outcome)
	}
	if len(h.n.restarts) != 0 || len(h.claims) != 1 || !h.claims[0].Probe {
		t.Fatalf("restarts %v claims %+v", h.n.restarts, h.claims)
	}
}

func TestAWorkerRestartsItsAgent(t *testing.T) {
	h := newHarness(t, 0)
	h.n.Runner = checktest.NewRunner()
	h.n.File("/var/lib/rancher/rke2/agent/client-kubelet.crt", checktest.CertPEM(t, "system:node", now0.Add(5*24*time.Hour), false))
	h.n.Expect("systemctl is-active rke2-agent", "active")
	h.n.onRestart = func() {
		h.n.File("/var/lib/rancher/rke2/agent/client-kubelet.crt", checktest.CertPEM(t, "system:node", now0.Add(365*24*time.Hour), false))
	}
	r := h.run(false)
	if r.Outcome != Restarted || !r.Renewed || h.n.restarts[0] != "rke2-agent" || !h.logged("no kubeconfig here") {
		t.Fatalf("%+v %v %q", r, h.n.restarts, h.log)
	}
}
