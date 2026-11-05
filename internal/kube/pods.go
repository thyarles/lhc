package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/runner"
)

// Pod is what the cluster API says about one pod.
type Pod struct {
	Namespace, Name, Node string
	// Owner is "Deployment team-a/api" (a ReplicaSet is shown as its
	// Deployment), "StatefulSet team-a/db"; "" for a bare pod.
	Owner      string
	Containers map[string]Container
	// Annotations are the pod's own, copied from its template; the large
	// kubectl last-applied-configuration is left out.
	Annotations map[string]string
}

// Container is one container's memory settings and last termination.
type Container struct {
	Limit, Request string // memory; "" when not set
	Restarts       int
	LastReason     string // "OOMKilled", "Error", …; "" when it never ended
	LastFinished   time.Time
}

// pods is the part of `get pods -o json` that is decoded.
type pods struct {
	Items []struct {
		Metadata struct {
			Namespace       string      `json:"namespace"`
			Name            string      `json:"name"`
			UID             string      `json:"uid"`
			Annotations     annotations `json:"annotations"`
			OwnerReferences []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name      string `json:"name"`
				Resources struct {
					Limits   map[string]string `json:"limits"`
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int    `json:"restartCount"`
				LastState    struct {
					Terminated *struct {
						Reason     string    `json:"reason"`
						FinishedAt time.Time `json:"finishedAt"`
					} `json:"terminated"`
				} `json:"lastState"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// Pods reads every pod in the cluster with one call, by UID.
func (c Cluster) Pods(ctx context.Context) (map[string]Pod, error) {
	res := c.Get(ctx, "pods", "--all-namespaces", "-o", "json")
	if !res.OK() {
		return nil, failure(res)
	}
	var list pods
	if err := json.Unmarshal([]byte(res.Stdout), &list); err != nil {
		return nil, errors.New("kubectl get pods: unreadable output")
	}
	out := make(map[string]Pod, len(list.Items))
	for _, it := range list.Items {
		m := it.Metadata
		p := Pod{Namespace: m.Namespace, Name: m.Name, Node: it.Spec.NodeName, Containers: map[string]Container{}, Annotations: m.Annotations.strings()}
		if len(m.OwnerReferences) > 0 {
			o := m.OwnerReferences[0]
			kind, name := o.Kind, o.Name
			if kind == "ReplicaSet" {
				// api-6b7c9d → Deployment api, without a second call.
				if i := strings.LastIndex(name, "-"); i > 0 {
					kind, name = "Deployment", name[:i]
				}
			}
			p.Owner = kind + " " + m.Namespace + "/" + name
		}
		for _, ct := range it.Spec.Containers {
			p.Containers[ct.Name] = Container{Limit: ct.Resources.Limits["memory"], Request: ct.Resources.Requests["memory"]}
		}
		for _, cs := range it.Status.ContainerStatuses {
			ct := p.Containers[cs.Name]
			ct.Restarts = cs.RestartCount
			if t := cs.LastState.Terminated; t != nil {
				ct.LastReason, ct.LastFinished = t.Reason, t.FinishedAt
			}
			p.Containers[cs.Name] = ct
		}
		out[m.UID] = p
	}
	return out, nil
}

// failure is the first line of what kubectl said when a call failed.
func failure(res runner.Result) error {
	msg := strings.TrimSpace(res.Stderr + "\n" + res.Stdout)
	if msg == "" && res.Err != nil {
		msg = res.Err.Error()
	}
	line, _, _ := strings.Cut(msg, "\n")
	if line == "" {
		line = "kubectl failed"
	}
	return errors.New(line)
}

// lastApplied is the copy of the whole object kubectl apply leaves on it:
// large, and never what lhc looks for.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// annotations holds an object's annotations undecoded, so a large value
// costs nothing until one is asked for.
type annotations map[string]json.RawMessage

// get is one annotation's value; "" when it is missing or not a string.
func (a annotations) get(key string) string {
	var v string
	if raw, ok := a[key]; ok && json.Unmarshal(raw, &v) == nil {
		return v
	}
	return ""
}

// strings decodes every annotation but lastApplied.
func (a annotations) strings() map[string]string {
	out := map[string]string{}
	for k := range a {
		if k != lastApplied {
			if v := a.get(k); v != "" {
				out[k] = v
			}
		}
	}
	return out
}
