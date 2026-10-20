package cri

import (
	"context"
	"slices"
	"testing"

	"github.com/thyarles/lhc/internal/checktest"
)

const id = "4f1d0c3b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a392817060000"

const inspect = `{"status":{"labels":{"io.kubernetes.pod.name":"csi-s3-66l84","io.kubernetes.pod.namespace":"infra-datashim"}}}`

func TestThePodIsFoundFromAnyCgroupLayout(t *testing.T) {
	for name, cgroup := range map[string]string{
		"containerd, systemd driver, cgroup v2": "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1a2b3c4d_5e6f.slice/cri-containerd-" + id + ".scope\n",
		"containerd, cgroupfs, cgroup v1":       "12:pids:/kubepods/burstable/pod1a2b3c4d-5e6f/" + id + "\n1:name=systemd:/kubepods/burstable/pod1a2b3c4d-5e6f/" + id + "\n",
		"CRI-O":                                 "0::/kubepods.slice/kubepods-pod1a2b.slice/crio-" + id + ".scope\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := checktest.NewRunner().Tool("crictl")
			r.File("/proc/42/cgroup", cgroup)
			r.Expect("inspect "+id, inspect)
			if got := Pod(context.Background(), r, "42"); got != "infra-datashim/csi-s3-66l84" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestNoPodIsNamedWithoutOne(t *testing.T) {
	for name, setup := range map[string]func(r *checktest.Runner){
		"a host process": func(r *checktest.Runner) {
			r.Tool("crictl").File("/proc/42/cgroup", "0::/system.slice/sshd.service\n")
		},
		"no crictl": func(r *checktest.Runner) {
			r.File("/proc/42/cgroup", "0::/kubepods.slice/cri-containerd-"+id+".scope\n")
		},
		"crictl fails": func(r *checktest.Runner) {
			r.Tool("crictl").File("/proc/42/cgroup", "0::/kubepods.slice/cri-containerd-"+id+".scope\n")
			r.Expect("inspect", "").Code(1)
		},
		"no pod labels": func(r *checktest.Runner) {
			r.Tool("crictl").File("/proc/42/cgroup", "0::/kubepods.slice/cri-containerd-"+id+".scope\n")
			r.Expect("inspect", `{"status":{"labels":{}}}`)
		},
		"the process is gone": func(r *checktest.Runner) { r.Tool("crictl") },
	} {
		t.Run(name, func(t *testing.T) {
			r := checktest.NewRunner()
			setup(r)
			if got := Pod(context.Background(), r, "42"); got != "" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestCrictlIsFoundWhereRke2PutsIt(t *testing.T) {
	r := checktest.NewRunner().Tool("/var/lib/rancher/rke2/bin/crictl")
	if got := Bin(r); got != "/var/lib/rancher/rke2/bin/crictl" {
		t.Fatalf("got %q", got)
	}
	r.Socket("/run/k3s/containerd/containerd.sock")
	if got := Args(r, "images"); !slices.Equal(got, []string{"--runtime-endpoint", "unix:///run/k3s/containerd/containerd.sock", "images"}) {
		t.Fatalf("args %q", got)
	}
	if got := Args(checktest.NewRunner(), "images"); !slices.Equal(got, []string{"images"}) {
		t.Fatalf("args without a socket %q", got)
	}
}
