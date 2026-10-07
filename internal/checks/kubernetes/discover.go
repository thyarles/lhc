package kubernetes

import (
	"os"

	"github.com/thyarles/lhc-go/internal/runner"
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

// Same story for crictl. On k3s/rke2 it is a symlink to the server binary
// that dispatches on argv[0].
var crictlPaths = []string{
	"/var/lib/rancher/rke2/bin/crictl",
	"/usr/local/bin/crictl",
	"/usr/bin/crictl",
	"/opt/bin/crictl",
}

// rke2 keeps k3s's containerd socket path.
var criSockets = []string{
	"/run/k3s/containerd/containerd.sock",
	"/run/containerd/containerd.sock",
	"/var/run/crio/crio.sock",
}

// Order matters: the node's own admin config beats a ~/.kube/config a human
// copied there and forgot about.
var kubeconfigPaths = []string{
	"/etc/rancher/rke2/rke2.yaml",
	"/etc/rancher/k3s/k3s.yaml",
	"/etc/kubernetes/admin.conf",
	"/root/.kube/config",
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

func crictlBin(r runner.Runner) string { return findTool(r, "crictl", crictlPaths) }

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

func criSocket(r runner.Runner) string {
	for _, s := range criSockets {
		if r.IsSocket(s) {
			return s
		}
	}
	return ""
}
