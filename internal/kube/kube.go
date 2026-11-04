// Package kube finds kubectl and the kubeconfig it talks through, and runs
// it read-only by construction: the only way to run kubectl is Get, which
// puts "get" first and refuses anything that could change the cluster or
// act as someone else. Nothing else in lhc runs kubectl (see guard_test).
package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/runner"
)

// Getenv reads $KUBECONFIG and $HOME. A variable so tests can replace the
// process environment, which the Runner does not expose.
var Getenv = os.Getenv

// Where kubectl actually lands. This list exists because of cron: the managed
// entry sets no PATH and cron does not read a login profile, so the nightly
// run sees PATH=/usr/bin:/bin. The rke2 control planes export
// PATH=/var/lib/rancher/rke2/bin only in an interactive profile. The real
// runner already searches these directories; the absolute paths keep
// discovery right even if that list ever changes.
var kubectlPaths = []string{
	"/var/lib/rancher/rke2/bin/kubectl",
	"/usr/local/bin/kubectl", // k3s
	"/usr/bin/kubectl",
	"/opt/bin/kubectl",
	"/snap/bin/kubectl",
}

// Order matters: the node's own admin config beats a ~/.kube/config a human
// copied there and forgot about.
var kubeconfigPaths = []string{
	"/etc/rancher/rke2/rke2.yaml",
	"/etc/rancher/k3s/k3s.yaml",
	"/etc/kubernetes/admin.conf",
	"/root/.kube/config",
}

const (
	// RequestTimeout goes to kubectl itself (--request-timeout), so a
	// kubectl still retrying the API gives up on its own.
	RequestTimeout = 15 * time.Second
	// callTimeout bounds each kubectl process from the outside.
	callTimeout = RequestTimeout + 10*time.Second
)

// Cluster is kubectl and the kubeconfig it uses; either may be missing.
type Cluster struct {
	r          runner.Runner
	bin        string
	Kubeconfig string // "" when none is readable
}

// Find looks for kubectl and a readable kubeconfig: explicit first (the
// checks.kubernetes.kubeconfig setting), then $KUBECONFIG, the well-known
// paths and $HOME/.kube/config.
func Find(r runner.Runner, explicit string) Cluster {
	return Cluster{r: r, bin: kubectlBin(r), Kubeconfig: kubeconfigPath(r, explicit)}
}

// HasKubectl says whether kubectl was found.
func (c Cluster) HasKubectl() bool { return c.bin != "" }

// Ready says whether there is both a kubectl and a kubeconfig to run it with.
func (c Cluster) Ready() bool { return c.bin != "" && c.Kubeconfig != "" }

// Get runs `kubectl --kubeconfig … --request-timeout=… get ARGS…`. An
// argument list that holds another verb, impersonation, a raw API path or
// a file to apply is refused without running anything.
func (c Cluster) Get(ctx context.Context, args ...string) runner.Result {
	if err := refused(args); err != nil {
		return runner.Result{Code: -1, Err: err}
	}
	if !c.Ready() {
		return runner.Result{Code: -1, Err: errors.New("kubectl or kubeconfig not found")}
	}
	ctx, cancel := runner.WithTimeout(ctx, callTimeout)
	defer cancel()
	full := []string{"--kubeconfig", c.Kubeconfig, fmt.Sprintf("--request-timeout=%ds", int(RequestTimeout/time.Second)), "get"}
	return c.r.Run(ctx, c.bin, append(full, args...)...)
}

// verbs are kubectl's commands other than get. Get would read one as a
// resource name, but an argument list holding one was meant for something
// else.
var verbs = []string{
	"create", "apply", "delete", "edit", "patch", "replace", "scale", "autoscale",
	"rollout", "label", "annotate", "set", "expose", "run", "exec", "attach", "cp",
	"port-forward", "proxy", "debug", "drain", "cordon", "uncordon", "taint",
	"certificate", "config", "plugin", "kustomize", "wait", "auth",
}

// refusedFlags act as someone else (--as, --as-group, --as-uid), reach past
// the resource API (--raw), or name manifests (-f, -k).
var refusedFlags = []string{"--as", "--raw", "--filename", "--kustomize", "--overrides", "--token", "--username", "--password"}

func refused(args []string) error {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch {
		case slices.Contains(verbs, a):
			return fmt.Errorf("kubectl %q refused: lhc only reads the cluster", a)
		case slices.ContainsFunc(refusedFlags, func(f string) bool { return strings.HasPrefix(name, f) }):
			return fmt.Errorf("kubectl flag %q refused: lhc only reads the cluster", name)
		case !strings.HasPrefix(a, "--") && strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && strings.ContainsAny(a[1:], "fk"):
			// -f, -k, and the same letters in a short-flag bundle (-Af).
			return fmt.Errorf("kubectl flag %q refused: lhc only reads the cluster", a)
		}
	}
	return nil
}

// findTool returns name from the runner's PATH, else the first absolute
// candidate that is executable, else "".
func findTool(r runner.Runner, name string, candidates []string) string {
	if p, ok := r.LookPath(name); ok {
		return p
	}
	for _, c := range candidates {
		if p, ok := r.LookPath(c); ok {
			return p
		}
	}
	return ""
}

func kubectlBin(r runner.Runner) string { return findTool(r, "kubectl", kubectlPaths) }

// kubeconfigPath is the first readable candidate. Readable, not merely
// present: an unreadable kubeconfig is a permanent condition, never a daily
// CAUTION.
func kubeconfigPath(r runner.Runner, explicit string) string {
	cands := []string{explicit, Getenv("KUBECONFIG")}
	cands = append(cands, kubeconfigPaths...)
	if home := Getenv("HOME"); home != "" {
		cands = append(cands, home+"/.kube/config")
	}
	for _, c := range cands {
		if c != "" && r.Readable(c) {
			return c
		}
	}
	return ""
}
