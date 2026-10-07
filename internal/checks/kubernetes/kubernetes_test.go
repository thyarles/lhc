package kubernetes

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
	"github.com/thyarles/lhc-go/internal/runner"
	"github.com/thyarles/lhc-go/internal/state"
)

const (
	nodesCmd  = "get nodes -o jsonpath"
	podsCmd   = "get pods --all-namespaces"
	pvcCmd    = "get pvc --all-namespaces"
	eventsCmd = "get events --all-namespaces"

	rke2Kubectl = "/var/lib/rancher/rke2/bin/kubectl"
	rke2Config  = "/etc/rancher/rke2/rke2.yaml"

	oneNode = "k3s-01||v1.30.5+k3s1|" +
		"MemoryPressure=False,DiskPressure=False,PIDPressure=False,Ready=True,|" +
		"k3s-01,10.0.0.5,"
)

// newEnv builds an Env with an empty process environment, so a developer's
// own $KUBECONFIG or $HOME can never change a result.
func newEnv(t *testing.T, mutate ...func(*Config)) *checktest.Env {
	t.Helper()
	setenv(t, map[string]string{})
	cfg := Check{}.Defaults().(*Config)
	for _, m := range mutate {
		m(cfg)
	}
	return checktest.NewEnv(t, Check{}, cfg)
}

func setenv(t *testing.T, vars map[string]string) {
	t.Helper()
	old := Getenv
	Getenv = func(k string) string { return vars[k] }
	t.Cleanup(func() { Getenv = old })
}

// found wires up discovery the rke2 way: kubectl only at its absolute path
// (not on PATH, as under cron), and the node's own kubeconfig.
func found(e *checktest.Env) {
	e.Fake.Tool(rke2Kubectl)
	e.Fake.File(rke2Config, "apiVersion: v1\n")
}

// cluster is a reachable cluster with nothing else to report. This host is
// matched to its node by address, not by name.
func cluster(e *checktest.Env, nodes string) {
	found(e)
	e.Fake.Expect(nodesCmd, nodes)
	e.Fake.Expect("hostname -I", "10.0.0.5")
}

func statusOf(t *testing.T, s *check.Section, label string) check.Status {
	t.Helper()
	for _, r := range s.Rows {
		if r.Label == label {
			return r.Status
		}
	}
	t.Fatalf("no row labelled %q in %q", label, checktest.Labels(s))
	return check.OK
}

func alertCount(t *testing.T, s *check.Section, want int) {
	t.Helper()
	if len(s.Alerts) != want {
		t.Fatalf("want %d alert(s), got %q", want, s.AlertMsgs())
	}
}

// ─── Discovery: the cron environment is the one that matters ───────────────

func TestKubectlIsFoundByAbsolutePathWhenNotOnPath(t *testing.T) {
	// The rke2 case: PATH=/usr/bin:/bin under cron, kubectl in
	// /var/lib/rancher. LookPath("kubectl") finds nothing.
	e := newEnv(t)
	cluster(e, oneNode)
	s := e.Run(Check{})
	if !s.Applicable {
		t.Fatalf("section not applicable: %+v", s.Rows)
	}
	if !e.Fake.Ran(rke2Kubectl + " --kubeconfig " + rke2Config) {
		t.Fatalf("kubectl not run by absolute path: %q", e.Fake.Calls())
	}
	if got := statusOf(t, s, "k3s-01 (this host)"); got != check.OK {
		t.Fatalf("this host: %v", got)
	}
}

func TestKubectlOnPathIsPreferred(t *testing.T) {
	e := newEnv(t)
	e.Fake.Tool("kubectl", rke2Kubectl)
	e.Fake.File(rke2Config, "x")
	e.Fake.Expect(nodesCmd, oneNode)
	e.Run(Check{})
	if !e.Fake.Ran("/usr/bin/kubectl --kubeconfig") || e.Fake.Ran(rke2Kubectl) {
		t.Fatalf("calls: %q", e.Fake.Calls())
	}
}

func TestKubeconfigDiscoveryOrder(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		env      map[string]string
		files    []string
		unread   []string
		want     string
	}{
		{name: "explicit beats everything", explicit: "/srv/kc", files: []string{"/srv/kc", "/env/kc", rke2Config}, env: map[string]string{"KUBECONFIG": "/env/kc"}, want: "/srv/kc"},
		{name: "$KUBECONFIG beats the well-known paths", files: []string{"/env/kc", rke2Config}, env: map[string]string{"KUBECONFIG": "/env/kc"}, want: "/env/kc"},
		{name: "node admin config beats a copied ~/.kube/config", files: []string{"/etc/kubernetes/admin.conf", "/root/.kube/config"}, want: "/etc/kubernetes/admin.conf"},
		{name: "$HOME is last", files: []string{"/home/ops/.kube/config"}, env: map[string]string{"HOME": "/home/ops"}, want: "/home/ops/.kube/config"},
		{name: "an unreadable file is skipped", files: []string{"/etc/rancher/k3s/k3s.yaml"}, unread: []string{rke2Config}, want: "/etc/rancher/k3s/k3s.yaml"},
		{name: "a missing explicit path falls through", explicit: "/nope", files: []string{rke2Config}, want: rke2Config},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setenv(t, c.env)
			f := checktest.NewRunner()
			for _, p := range c.files {
				f.File(p, "x")
			}
			for _, p := range c.unread {
				f.Unreadable(p)
			}
			if got := kubeconfigPath(f, c.explicit); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestNoKubectlAndNoKubeconfigIsSilent(t *testing.T) {
	// A plain web server must not be told about Kubernetes, ever.
	e := newEnv(t)
	s := e.Run(Check{})
	if s.Applicable {
		t.Fatal("section applicable on a non-Kubernetes host")
	}
	checktest.NoAlerts(t, s)
	if len(s.MissingTools) != 0 {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
}

func TestKubectlWithoutAKubeconfigIsSilent(t *testing.T) {
	// An agent node: kubectl exists, there is no cluster to ask. Permanent.
	e := newEnv(t)
	e.Fake.Tool("kubectl")
	s := e.Run(Check{})
	if s.Applicable {
		t.Fatal("section applicable without a kubeconfig")
	}
	checktest.NoAlerts(t, s)
}

func TestKubeconfigWithoutKubectlAsksForItQuietly(t *testing.T) {
	e := newEnv(t)
	e.Fake.File(rke2Config, "x")
	s := e.Run(Check{})
	if s.Status != check.Info || len(s.MissingTools) != 1 || !s.MissingTools[0].Optional {
		t.Fatalf("status %v, tools %+v", s.Status, s.MissingTools)
	}
	checktest.NoAlerts(t, s)
}

func TestDisabledCheckDoesNotRun(t *testing.T) {
	// Python's scope = off is enabled: false now; RunAll skips the check.
	e := newEnv(t, func(c *Config) { c.Enabled = false })
	cluster(e, oneNode)
	res := check.RunAll(context.Background(), []check.Check{Check{}},
		map[string]check.Config{"kubernetes": e.Config}, &state.Dir{Path: t.TempDir()}, *e.Env)
	if len(res.Sections) != 0 || len(e.Fake.Calls()) != 0 {
		t.Fatalf("disabled check ran: %d section(s), calls %q", len(res.Sections), e.Fake.Calls())
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		ok     bool
	}{
		{"defaults", func(*Config) {}, true},
		{"cluster", func(c *Config) { c.Scope = "cluster" }, true},
		{"node, any case", func(c *Config) { c.Scope = " Node " }, true},
		{"off is enabled: false now", func(c *Config) { c.Scope = "off" }, false},
		{"unknown scope", func(c *Config) { c.Scope = "everything" }, false},
		{"zero restart delta", func(c *Config) { c.RestartDeltaCaution = 0 }, false},
		{"zero max pods", func(c *Config) { c.MaxPods = 0 }, false},
		{"negative pending", func(c *Config) { c.PendingMinutes = -1 }, false},
	}
	for _, c := range cases {
		cfg := Check{}.Defaults().(*Config)
		c.mutate(cfg)
		if err := cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: Validate() = %v", c.name, err)
		}
	}
}

func TestUnreachableAPIIsOneCautionNotUnhealthy(t *testing.T) {
	e := newEnv(t)
	found(e)
	e.Fake.Expect(nodesCmd, "").Code(1).Stderr("The connection to the server 127.0.0.1:6443 was refused")
	s := e.Run(Check{})
	if s.Status != check.Caution {
		t.Fatalf("status %v", s.Status)
	}
	alertCount(t, s, 1)
	if !strings.Contains(s.Alerts[0].Msg, "unreachable") {
		t.Fatalf("alert: %q", s.Alerts[0].Msg)
	}
}

func TestRejectedCredentialsSaySo(t *testing.T) {
	e := newEnv(t)
	found(e)
	e.Fake.Expect(nodesCmd, "error: You must be logged in to the server (Unauthorized)").Code(1)
	s := e.Run(Check{})
	if !checktest.HasAlertContaining(s, "credentials") {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
}

// argvRecorder keeps each command's exact argv, which the fake's joined
// command lines cannot show.
type argvRecorder struct {
	*checktest.Runner
	argv [][]string
}

func (r *argvRecorder) Run(ctx context.Context, name string, args ...string) runner.Result {
	r.argv = append(r.argv, append([]string{name}, args...))
	return r.Runner.Run(ctx, name, args...)
}

func TestJSONPathTemplateIsOneVerbatimArgument(t *testing.T) {
	// The Python version once wrote {'|'} inside a single-quoted shell word,
	// which closed the quote and turned the separator into a pipe. With no
	// shell, the template must reach kubectl untouched as a single argument.
	e := newEnv(t)
	cluster(e, oneNode)
	rec := &argvRecorder{Runner: e.Fake}
	e.Runner = rec
	e.Run(Check{})
	want := []string{
		rke2Kubectl, "--kubeconfig", rke2Config, "--request-timeout=15s",
		"get", "nodes", "-o", `jsonpath={range .items[*]}{.metadata.name}{"|"}{.spec.unschedulable}{"|"}` +
			`{.status.nodeInfo.kubeletVersion}{"|"}{range .status.conditions[*]}{.type}{"="}{.status}{","}{end}{"|"}` +
			`{range .status.addresses[*]}{.address}{","}{end}{"\n"}{end}`,
		"--allow-missing-template-keys=true",
	}
	if len(rec.argv) == 0 || !slices.Equal(rec.argv[0], want) {
		t.Fatalf("first argv:\n got %q\nwant %q", rec.argv[0], want)
	}
	for _, a := range rec.argv {
		for _, arg := range a {
			if strings.Contains(arg, "'") {
				t.Fatalf("shell quoting leaked into an argument: %q", a)
			}
		}
	}
}

func TestThisNodeIsMatchedByHostnameFirst(t *testing.T) {
	e := newEnv(t)
	e.Host = check.Host{Label: "K3S-02.example.com", Kernel: "K3S-02.example.com", Resolved: "vip.example.com"}
	found(e)
	e.Fake.Expect(nodesCmd, "k3s-01||v1|Ready=True,|10.0.0.5,\nk3s-02||v1|Ready=True,|10.0.0.6,")
	e.Fake.Expect("hostname -I", "10.0.0.5")
	s := e.Run(Check{})
	if !strings.HasPrefix(checktest.Rows(s)["This Node"].Value, "k3s-02") {
		t.Fatalf("This Node: %+v", checktest.Rows(s)["This Node"])
	}
	if e.Fake.Ran("hostname -I") {
		t.Fatal("address fallback used although the name matched")
	}
}

func TestAJumpHostIsNotANode(t *testing.T) {
	e := newEnv(t)
	found(e)
	e.Fake.Expect(nodesCmd, oneNode)
	e.Fake.Expect("hostname -I", "192.168.1.9")
	s := e.Run(Check{})
	if got := checktest.Rows(s)["This Node"]; got.Status != check.Info || !strings.Contains(got.Value, "admin/jump host") {
		t.Fatalf("This Node: %+v", got)
	}
	if statusOf(t, s, "k3s-01") != check.OK {
		t.Fatal("node row missing")
	}
}

// ─── Per-signal flags ──────────────────────────────────────────────────────

func TestEachSignalCanBeSwitchedOff(t *testing.T) {
	cases := []struct {
		flag string
		off  func(*Config)
		cmd  string
	}{
		{"pods", func(c *Config) { c.Pods = false }, podsCmd},
		{"pvcs", func(c *Config) { c.PVCs = false }, pvcCmd},
		{"events", func(c *Config) { c.Events = false }, eventsCmd},
	}
	for _, c := range cases {
		t.Run(c.flag, func(t *testing.T) {
			e := newEnv(t, c.off)
			cluster(e, oneNode)
			e.Run(Check{})
			if e.Fake.Ran(c.cmd) {
				t.Fatalf("%s ran with %s: false", c.cmd, c.flag)
			}
		})
	}
}
