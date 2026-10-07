// Package kubernetes reports on the cluster this host belongs to: nodes,
// pods, PVCs, warning events and the local image store.
//
// It reads the cluster by running kubectl (and crictl), never by linking the
// Kubernetes client library. Every command goes through env.Runner with an
// argument list, so there is no shell and nothing to quote.
//
// Two facts shape everything here:
//
//  1. Discovery must work under cron's environment, not the operator's. The
//     rke2 control planes get kubectl from an interactive `export PATH=...`
//     that the nightly run never sees.
//  2. A Kubernetes node is a noise machine. Completed helm pods, month-old
//     Evicted objects, PVCs that are Pending by design and pods that were
//     restarting when the cron fired must never reach anyone's inbox.
package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/runner"
)

func init() { check.Register(Check{}) }

// Config is the checks.kubernetes block.
type Config struct {
	check.Toggle `yaml:",inline"`
	// auto | cluster | node. "off" is spelled enabled: false.
	Scope string `yaml:"scope"`
	// Explicit kubeconfig, tried before $KUBECONFIG and the well-known paths.
	Kubeconfig string `yaml:"kubeconfig"`
	// Individual signals, so one noisy source can be silenced without losing
	// the rest of the section.
	Nodes  bool `yaml:"nodes"`
	Pods   bool `yaml:"pods"`
	PVCs   bool `yaml:"pvcs"`
	Events bool `yaml:"events"`
	Images bool `yaml:"images"`
	// Images churn on every deploy, so the default is a count only.
	ListImages bool `yaml:"list_images"`
	// A pod Pending/ContainerCreating for less than this is the scheduler
	// doing its job; past it, something is stuck.
	PendingMinutes float64 `yaml:"pending_minutes"`
	// Restarts picked up since the last run that make a pod CAUTION.
	RestartDeltaCaution int `yaml:"restart_delta_caution"`
	// Evictions younger than this are news; older Evicted objects are INFO.
	EvictedRecentHours float64 `yaml:"evicted_recent_hours"`
	// Guard rail: stop reading the pod list past this many rows.
	MaxPods int `yaml:"max_pods"`
}

// Check is the Kubernetes check.
type Check struct{}

func (Check) Meta() check.Meta { return check.Meta{Name: "kubernetes", Title: "Kubernetes", Order: 80} }

func (Check) Defaults() check.Config {
	return &Config{
		Toggle: check.On, Scope: "auto",
		Nodes: true, Pods: true, PVCs: true, Events: true, Images: true,
		PendingMinutes: 15, RestartDeltaCaution: 3, EvictedRecentHours: 24, MaxPods: 2000,
	}
}

func normScope(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Validate rejects values the YAML schema cannot.
func (c *Config) Validate() error {
	switch normScope(c.Scope) {
	case "auto", "cluster", "node":
	case "off":
		return fmt.Errorf(`scope "off" is no longer supported; set enabled: false instead`)
	default:
		return fmt.Errorf("scope %q is not one of auto, cluster, node", c.Scope)
	}
	if c.PendingMinutes < 0 {
		return fmt.Errorf("pending_minutes (%g) is negative", c.PendingMinutes)
	}
	if c.EvictedRecentHours < 0 {
		return fmt.Errorf("evicted_recent_hours (%g) is negative", c.EvictedRecentHours)
	}
	if c.RestartDeltaCaution < 1 {
		return fmt.Errorf("restart_delta_caution (%d) must be at least 1", c.RestartDeltaCaution)
	}
	if c.MaxPods < 1 {
		return fmt.Errorf("max_pods (%d) must be at least 1", c.MaxPods)
	}
	return nil
}

const (
	// requestTimeout goes to kubectl itself (--request-timeout), so a
	// kubectl still retrying the API gives up on its own.
	requestTimeout = 15 * time.Second
	// callTimeout bounds each kubectl process from the outside.
	callTimeout = requestTimeout + 10*time.Second
	// crictlTimeout bounds the local image listing.
	crictlTimeout = requestTimeout
	listCap       = 10
)

// kubectl's own wording, not errno strings: a refused connection reads "The
// connection to the server 127.0.0.1:6443 was refused - did you specify the
// right host or port?", which matches none of the obvious phrases.
var (
	unreachable = []string{
		"refused", "no such host", "i/o timeout", "timeout",
		"unable to connect to the server", "network is unreachable",
		"did you specify the right host", "tls handshake", "eof",
	}
	denied = []string{
		"forbidden", "unauthorized", "you must be logged in",
		"certificate has expired", "x509", "invalid bearer token",
	}
)

// kube runs kubectl against one kubeconfig.
type kube struct {
	r    runner.Runner
	bin  string
	base []string
}

func (k kube) run(ctx context.Context, args ...string) runner.Result {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return k.r.Run(ctx, k.bin, append(slices.Clone(k.base), args...)...)
}

// combined is stdout and stderr together, what `2>&1` used to give: kubectl
// reports most failures on stderr, "No resources found" included.
func combined(res runner.Result) string {
	return strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
}

// failure is the text that best explains a failed call.
func failure(res runner.Result, fallback string) string {
	if out := combined(res); out != "" {
		return out
	}
	if res.Err != nil {
		return res.Err.Error()
	}
	return fallback
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func containsAny(s string, subs []string) bool {
	return slices.ContainsFunc(subs, func(x string) bool { return strings.Contains(s, x) })
}

// moreSuffix is the " (+N more)" after a list cut to its first three.
func moreSuffix(n int) string {
	if n > 3 {
		return fmt.Sprintf(" (+%d more)", n-3)
	}
	return ""
}

func firstN[T any](xs []T, n int) []T { return xs[:min(n, len(xs))] }

func save(env *check.Env, key string, v any) {
	if err := env.State.Save(key, v); err != nil {
		env.Log.Warn("saving state", "key", key, "err", err)
	}
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("kubernetes", c.Meta().Title)
	scope := normScope(cfg.Scope)

	kubectl := kubectlBin(env.Runner)
	kcfg := kubeconfigPath(env.Runner, cfg.Kubeconfig)

	// A kubectl with nothing to talk to says nothing worth printing, and a
	// host that will never run Kubernetes must not be told to install it.
	switch {
	case kubectl == "" && kcfg == "":
		s.NotApplicable("Not a Kubernetes node")
		return s
	case kcfg == "":
		s.NotApplicable("kubectl present but no readable kubeconfig (agent node or client-only host)")
		return s
	case kubectl == "":
		s.Add("kubectl", fmt.Sprintf("kubeconfig at %s but kubectl is not installed", kcfg), check.Info)
		s.NeedTool(check.Tool{Name: "kubectl", RHELPkg: "kubernetes-client", DebPkg: "kubectl", Optional: true})
		return s
	}

	k := kube{r: env.Runner, bin: kubectl, base: []string{
		"--kubeconfig", kcfg, fmt.Sprintf("--request-timeout=%ds", int(requestTimeout/time.Second)),
	}}

	// The node query IS the reachability probe: no wasted round trip, and no
	// `get --raw /readyz`, which needs a nonResourceURL grant a restricted
	// kubeconfig may not have.
	nodes, errText := getNodes(ctx, k)
	if errText != "" {
		low := strings.ToLower(errText)
		var why string
		switch {
		case containsAny(low, denied):
			why = "credentials rejected by the API server"
		case containsAny(low, unreachable):
			why = "API server unreachable"
		default:
			why = "kubectl failed: " + clip(firstLine(errText), 80)
		}
		s.Add("Cluster API", why, check.Caution, check.Detail(kcfg))
		s.Alert(check.Caution, fmt.Sprintf("Kubernetes API not usable from this host (%s)", why))
		return s
	}

	me := thisNode(ctx, env, nodes)
	if me != nil {
		s.Add("This Node", fmt.Sprintf("%s  (%s)", me.name, me.version), check.OK,
			check.Detail(fmt.Sprintf("%d node(s) in the cluster", len(nodes))))
	} else {
		s.Add("This Node", "not a node in this cluster (admin/jump host)", check.Info)
	}

	if cfg.Nodes {
		nodeRows(s, env, nodes, me)
	}
	nodeScoped := scope == "node" || (scope == "auto" && me != nil && len(nodes) > 1)
	if cfg.Pods {
		var on *node
		if nodeScoped {
			on = me
		}
		podRows(ctx, s, env, cfg, k, on)
	}
	if cfg.PVCs && scope != "node" {
		pvcRows(ctx, s, env, k)
	}
	if cfg.Events {
		eventRows(ctx, s, env, k)
	}
	if cfg.Images {
		imageRows(ctx, s, env, cfg)
	}
	return s
}

// thisNode finds which node is this host.
//
// Node names are usually the lowercased short hostname, but --node-name and
// cloud providers break that, so fall back to matching an address the node
// reports against this host's own IPs.
func thisNode(ctx context.Context, env *check.Env, nodes []node) *node {
	names := map[string]bool{}
	for _, n := range []string{env.Host.Resolved, env.Host.Kernel, env.Host.Label} {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		short, _, _ := strings.Cut(n, ".")
		names[n], names[short] = true, true
	}
	for i := range nodes {
		if names[strings.ToLower(nodes[i].name)] {
			return &nodes[i]
		}
	}
	mine := strings.Fields(env.Runner.Run(ctx, "hostname", "-I").Stdout)
	for i := range nodes {
		for _, ip := range mine {
			if nodes[i].addresses[ip] {
				return &nodes[i]
			}
		}
	}
	return nil
}
