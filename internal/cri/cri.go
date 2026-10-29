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

// ContainerID is the 64-hex container ID in a cgroup path, or "".
func ContainerID(path string) string { return containerID.FindString(path) }

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
	c, ok := Container(ctx, r, string(id))
	if !ok {
		return ""
	}
	return c.Pod()
}

// Ref is a container and the pod it belongs to.
type Ref struct {
	Namespace, Name string // the pod
	Container       string // "" when only the pod is known
}

// Pod is "namespace/name".
func (c Ref) Pod() string { return c.Namespace + "/" + c.Name }

// Container names the pod and container of a container ID. It works as long
// as the runtime still holds the container, exited ones included; false
// when crictl is missing, fails, or the container has no pod labels.
func Container(ctx context.Context, r runner.Runner, id string) (Ref, bool) {
	var c struct {
		Status struct {
			Labels map[string]string `json:"labels"`
		} `json:"status"`
	}
	if !crictl(ctx, r, &c, "inspect", id) {
		return Ref{}, false
	}
	l := c.Status.Labels
	ref := Ref{l["io.kubernetes.pod.namespace"], l["io.kubernetes.pod.name"], l["io.kubernetes.container.name"]}
	return ref, ref.Namespace != "" && ref.Name != ""
}

// PodByUID names the pod with this UID while it still runs on the node,
// even after the container that was killed is gone.
func PodByUID(ctx context.Context, r runner.Runner, uid string) (Ref, bool) {
	var out struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if !crictl(ctx, r, &out, "pods", "--label", "io.kubernetes.pod.uid="+uid, "-o", "json") || len(out.Items) == 0 {
		return Ref{}, false
	}
	m := out.Items[0].Metadata
	return Ref{Namespace: m.Namespace, Name: m.Name}, m.Namespace != "" && m.Name != ""
}

// crictl runs one crictl command and decodes its JSON into v.
func crictl(ctx context.Context, r runner.Runner, v any, args ...string) bool {
	bin := Bin(r)
	if bin == "" {
		return false
	}
	ctx, cancel := runner.WithTimeout(ctx, Timeout)
	defer cancel()
	res := r.Run(ctx, bin, Args(r, args...)...)
	return res.OK() && json.Unmarshal([]byte(res.Stdout), v) == nil
}
