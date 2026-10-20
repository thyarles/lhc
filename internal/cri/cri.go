// Package cri finds the node's crictl and container runtime socket, and
// names the Kubernetes pod a process runs in.
package cri

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/thyarles/lhc/internal/runner"
)

// Where crictl lands. Cron runs with PATH=/usr/bin:/bin, and rke2 adds its
// bin directory only in an interactive profile. On k3s/rke2 crictl is a
// symlink to the server binary that dispatches on argv[0].
var crictlPaths = []string{
	"/var/lib/rancher/rke2/bin/crictl",
	"/usr/local/bin/crictl",
	"/usr/bin/crictl",
	"/opt/bin/crictl",
}

// rke2 keeps k3s's containerd socket path.
var sockets = []string{
	"/run/k3s/containerd/containerd.sock",
	"/run/containerd/containerd.sock",
	"/var/run/crio/crio.sock",
}

// Bin is crictl from the runner's PATH, else the first known location, else "".
func Bin(r runner.Runner) string {
	if p, ok := r.LookPath("crictl"); ok {
		return p
	}
	for _, c := range crictlPaths {
		if p, ok := r.LookPath(c); ok {
			return p
		}
	}
	return ""
}

// Socket is the first container runtime socket present, or "".
func Socket(r runner.Runner) string {
	for _, s := range sockets {
		if r.IsSocket(s) {
			return s
		}
	}
	return ""
}

// Args are crictl's arguments with the runtime endpoint first when known:
// without it, crictl on rke2 looks for the stock containerd socket and fails.
func Args(r runner.Runner, args ...string) []string {
	if sock := Socket(r); sock != "" {
		return append([]string{"--runtime-endpoint", "unix://" + sock}, args...)
	}
	return args
}

// Timeout bounds one crictl call.
const Timeout = 10 * time.Second

// containerID is the 64-hex ID in a cgroup path: containerd's
// cri-containerd-<id>.scope, CRI-O's crio-<id>.scope, or a cgroupfs
// .../<id>. A pod UID has dashes or underscores, so it never matches.
var containerID = regexp.MustCompile(`[0-9a-f]{64}`)

// Pod is "namespace/name" of the pod that pid runs in, or "" when it is not
// in a pod or crictl cannot say.
func Pod(ctx context.Context, r runner.Runner, pid string) string {
	b, err := r.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return ""
	}
	id := containerID.Find(b)
	if id == nil {
		return ""
	}
	bin := Bin(r)
	if bin == "" {
		return ""
	}
	ctx, cancel := runner.WithTimeout(ctx, Timeout)
	defer cancel()
	res := r.Run(ctx, bin, Args(r, "inspect", string(id))...)
	if !res.OK() {
		return ""
	}
	var c struct {
		Status struct {
			Labels map[string]string `json:"labels"`
		} `json:"status"`
	}
	if json.Unmarshal([]byte(res.Stdout), &c) != nil {
		return ""
	}
	ns, name := c.Status.Labels["io.kubernetes.pod.namespace"], c.Status.Labels["io.kubernetes.pod.name"]
	if ns == "" || name == "" {
		return ""
	}
	return ns + "/" + name
}
