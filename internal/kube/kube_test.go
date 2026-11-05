package kube

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/checktest"
)

const rke2Config = "/etc/rancher/rke2/rke2.yaml"

func setenv(t *testing.T, vars map[string]string) {
	t.Helper()
	old := Getenv
	Getenv = func(k string) string { return vars[k] }
	t.Cleanup(func() { Getenv = old })
}

// ready is an rke2 control plane: kubectl only at its absolute path, as
// under cron, and the node's own kubeconfig.
func ready(t *testing.T) (*checktest.Runner, Cluster) {
	setenv(t, map[string]string{})
	r := checktest.NewRunner().Tool("/var/lib/rancher/rke2/bin/kubectl").File(rke2Config, "apiVersion: v1\n")
	return r, Find(r, "")
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
			if got := Find(f, c.explicit).Kubeconfig; got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestGetOnlyEverRunsGet(t *testing.T) {
	r, c := ready(t)
	res := c.Get(context.Background(), "pods", "--all-namespaces", "-o", "json")
	if !res.OK() {
		t.Fatal(res.Err)
	}
	want := "/var/lib/rancher/rke2/bin/kubectl --kubeconfig " + rke2Config + " --request-timeout=15s get pods --all-namespaces -o json"
	if calls := r.Calls(); !slices.Equal(calls, []string{want}) {
		t.Fatalf("calls %q", calls)
	}
}

func TestGetRefusesAnythingButReading(t *testing.T) {
	for _, args := range [][]string{
		{"pods", "delete"},
		{"apply"},
		{"patch", "deploy/api"},
		{"exec", "api-0"},
		{"pods", "--as", "system:admin"},
		{"pods", "--as=system:admin"},
		{"pods", "--as-group=system:masters"},
		{"--raw", "/api/v1/namespaces/default/pods"},
		{"-f", "evil.yaml"},
		{"-Af"},
		{"--filename=evil.yaml"},
		{"-k", "dir"},
		{"--token=abc"},
	} {
		r, c := ready(t)
		if res := c.Get(context.Background(), args...); res.OK() || res.Err == nil || !strings.Contains(res.Err.Error(), "refused") {
			t.Errorf("%q: not refused: %+v", args, res)
		}
		if len(r.Calls()) != 0 {
			t.Errorf("%q: ran %q", args, r.Calls())
		}
	}
	for _, args := range [][]string{
		{"nodes", "-o", "jsonpath={.items[*].metadata.name}", "--allow-missing-template-keys=true"},
		{"events", "--all-namespaces", "--field-selector", "type=Warning"},
		{"pods", "-A", "-l", "app=frank", "--no-headers"},
	} {
		if err := refused(args); err != nil {
			t.Errorf("%q refused: %v", args, err)
		}
	}
}

func TestWithoutAKubeconfigNothingRuns(t *testing.T) {
	setenv(t, map[string]string{})
	r := checktest.NewRunner().Tool("kubectl")
	c := Find(r, "")
	if c.Ready() || !c.HasKubectl() || c.Kubeconfig != "" {
		t.Fatalf("cluster %+v", c)
	}
	if res := c.Get(context.Background(), "pods"); res.OK() || len(r.Calls()) != 0 {
		t.Fatalf("ran %q", r.Calls())
	}
}

const podsJSON = `{"apiVersion":"v1","kind":"List","items":[
{"metadata":{"namespace":"team-a","name":"api-6b7c9d-x2k4p","uid":"1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d","ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"api-6b7c9d","controller":true}]},
 "spec":{"nodeName":"node1","containers":[{"name":"app","resources":{"limits":{"cpu":"1","memory":"512Mi"},"requests":{"memory":"256Mi"}}},{"name":"sidecar","resources":{}}]},
 "status":{"containerStatuses":[{"name":"app","restartCount":3,"lastState":{"terminated":{"exitCode":137,"reason":"OOMKilled","finishedAt":"2026-01-05T03:15:02Z"}}},{"name":"sidecar","restartCount":0,"lastState":{}}]}},
{"metadata":{"namespace":"team-b","name":"db-0","uid":"9f8e7d6c-5b4a-3928-1706-f5e4d3c2b1a0","ownerReferences":[{"kind":"StatefulSet","name":"db"}]},
 "spec":{"nodeName":"node2","containers":[{"name":"postgres","resources":{"limits":{"memory":"2Gi"}}}]},"status":{}},
{"metadata":{"namespace":"default","name":"debug","uid":"00000000-0000-0000-0000-000000000001"},"spec":{"containers":[{"name":"sh"}]},"status":{}}
]}`

func TestPodsAreIndexedByUID(t *testing.T) {
	r, c := ready(t)
	r.Expect("get pods --all-namespaces -o json", podsJSON)
	got, err := c.Pods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	api := got["1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"]
	app := api.Containers["app"]
	if api.Owner != "Deployment team-a/api" || api.Node != "node1" || app.Limit != "512Mi" || app.Request != "256Mi" ||
		app.Restarts != 3 || app.LastReason != "OOMKilled" || app.LastFinished.IsZero() || api.Containers["sidecar"].Limit != "" {
		t.Fatalf("api: %+v", api)
	}
	if db := got["9f8e7d6c-5b4a-3928-1706-f5e4d3c2b1a0"]; db.Owner != "StatefulSet team-b/db" || db.Containers["postgres"].Limit != "2Gi" {
		t.Fatalf("db: %+v", db)
	}
	if bare := got["00000000-0000-0000-0000-000000000001"]; bare.Owner != "" || bare.Name != "debug" {
		t.Fatalf("bare pod: %+v", bare)
	}
}

func TestPodsSaysWhyTheAPIFailed(t *testing.T) {
	r, c := ready(t)
	r.Expect("get pods", "").Code(1).Stderr("The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?\nmore")
	_, err := c.Pods(context.Background())
	if err == nil || err.Error() != "The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?" {
		t.Fatalf("err %v", err)
	}
}

// bigApplied is the copy of the whole object `kubectl apply` leaves in
// an annotation, often tens of kilobytes.
var bigApplied = `"kubectl.kubernetes.io/last-applied-configuration":"{\"apiVersion\":\"apps/v1\",\"data\":\"` + strings.Repeat("x", 64<<10) + `\"}"`

var workloadsJSON = `{"apiVersion":"v1","kind":"List","items":[
{"kind":"Deployment","metadata":{"namespace":"team-a","name":"api","annotations":{` + bigApplied + `,"lhc/notify":"name.surname, other.person"}}},
{"kind":"StatefulSet","metadata":{"namespace":"team-b","name":"db"}},
{"kind":"DaemonSet","metadata":{"namespace":"kube-system","name":"agent","annotations":{"lhc/notify":7}}}
]}`

func TestWorkloadsReadOnlyTheAnnotationAskedFor(t *testing.T) {
	r, c := ready(t)
	r.Expect("get deployments,statefulsets,daemonsets --all-namespaces -o json", workloadsJSON)
	got, err := c.Workloads(context.Background(), "lhc/notify")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Deployment team-a/api":       "name.surname, other.person",
		"StatefulSet team-b/db":       "",
		"DaemonSet kube-system/agent": "", // not a string
	}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Fatalf("%s: got %q (%v), want %q", k, g, ok, v)
		}
	}
}

func TestNamespacesAndPodsCarryTheirAnnotations(t *testing.T) {
	r, c := ready(t)
	r.Expect("get namespaces -o json", `{"items":[{"kind":"Namespace","metadata":{"name":"team-a","annotations":{"lhc/notify":"lead"}}},{"kind":"Namespace","metadata":{"name":"default"}}]}`)
	r.Expect("get pods --all-namespaces -o json", `{"items":[{"metadata":{"namespace":"team-a","name":"api-1","uid":"u1","annotations":{`+bigApplied+`,"lhc/notify":"dev.one"}},"spec":{},"status":{}}]}`)
	ns, err := c.Namespaces(context.Background(), "lhc/notify")
	if err != nil || ns["team-a"] != "lead" || ns["default"] != "" || len(ns) != 2 {
		t.Fatalf("namespaces %q, %v", ns, err)
	}
	pods, err := c.Pods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a := pods["u1"].Annotations; a["lhc/notify"] != "dev.one" || len(a) != 1 {
		t.Fatalf("pod annotations %q", a)
	}
	if calls := strings.Join(r.Calls(), "\n"); strings.Count(calls, " get ") != 2 {
		t.Fatalf("calls:\n%s", calls)
	}
}

func TestWorkloadsSaysWhyTheAPIFailed(t *testing.T) {
	r, c := ready(t)
	r.Expect("get deployments", "").Code(1).Stderr(`Error from server (Forbidden): deployments.apps is forbidden: User "lhc" cannot list resource "deployments"`)
	if _, err := c.Workloads(context.Background(), "lhc/notify"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("err %v", err)
	}
	r.Expect("get namespaces", "not json")
	if _, err := c.Namespaces(context.Background(), "lhc/notify"); err == nil || err.Error() != "kubectl get namespaces: unreadable output" {
		t.Fatalf("err %v", err)
	}
}
