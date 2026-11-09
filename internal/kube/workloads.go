package kube

import (
	"context"
	"encoding/json"
	"errors"
)

// objects is the part of a `get <kinds> -o json` list that is decoded:
// kind, name and annotations, which stay undecoded until one is asked for.
type objects struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Namespace   string      `json:"namespace"`
			Name        string      `json:"name"`
			Annotations annotations `json:"annotations"`
		} `json:"metadata"`
	} `json:"items"`
}

func (c Cluster) objects(ctx context.Context, args ...string) (objects, error) {
	var list objects
	res := c.Get(ctx, append(args, "-o", "json")...)
	if !res.OK() {
		return list, failure(res)
	}
	if err := json.Unmarshal([]byte(res.Stdout), &list); err != nil {
		return list, errors.New("kubectl get " + args[0] + ": unreadable output")
	}
	return list, nil
}

// Workloads reads every Deployment, StatefulSet and DaemonSet with one
// call: "Deployment team-a/api" (the form of Pod.Owner) → the value of the
// annotation key, "" when the workload does not carry it.
func (c Cluster) Workloads(ctx context.Context, key string) (map[string]string, error) {
	list, err := c.objects(ctx, "deployments,statefulsets,daemonsets", "--all-namespaces")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list.Items))
	for _, it := range list.Items {
		m := it.Metadata
		out[it.Kind+" "+m.Namespace+"/"+m.Name] = m.Annotations.get(key)
	}
	return out, nil
}

// Namespaces reads every namespace: name → the value of the annotation
// key, "" when it does not carry it.
func (c Cluster) Namespaces(ctx context.Context, key string) (map[string]string, error) {
	list, err := c.objects(ctx, "namespaces")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list.Items))
	for _, it := range list.Items {
		out[it.Metadata.Name] = it.Metadata.Annotations.get(key)
	}
	return out, nil
}
