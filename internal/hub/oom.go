package hub

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/kube"
	"github.com/thyarles/lhc/internal/report"
)

// oomLines are the digest's lines about a host's OOM-killed pods, most
// kills first: what the host resolved, plus the memory limit and owner
// when the cluster could be asked (cluster non-nil).
//
//	OOM team-a/api-6b7c9d-x2k4p · container app · 3 kills · limit 512Mi · Deployment team-a/api
func oomLines(refs []check.PodRef, cluster map[string]kube.Pod) []string {
	refs = slices.DeleteFunc(slices.Clone(refs), func(r check.PodRef) bool { return r.Reason != "oom_killed" })
	slices.SortStableFunc(refs, func(a, b check.PodRef) int { return cmp.Compare(b.Count, a.Count) })
	byName := map[string]string{}
	for uid, p := range cluster {
		byName[p.Namespace+"/"+p.Name] = uid
	}
	var out []string
	for i, r := range refs {
		if i == report.DetailsListed-1 && len(refs) > report.DetailsListed {
			out = append(out, fmt.Sprintf("(+%d more OOM-killed pods)", len(refs)-i))
			break
		}
		out = append(out, oomLine(r, cluster, byName))
	}
	return out
}

func oomLine(r check.PodRef, cluster map[string]kube.Pod, byName map[string]string) string {
	name := r.Namespace + "/" + r.Name
	uid := r.UID
	if uid == "" {
		uid = byName[name]
	}
	p, found := cluster[uid]
	switch {
	case found:
		name = p.Namespace + "/" + p.Name
	case r.Name == "":
		first, _, _ := strings.Cut(r.UID, "-")
		name = "pod UID " + first + "-…"
	}
	container := r.Container
	if found && container == "" && len(p.Containers) == 1 {
		for c := range p.Containers {
			container = c
		}
	}
	parts := []string{"OOM " + name}
	if container != "" {
		parts = append(parts, "container "+container)
	}
	if cluster == nil && r.Process != "" {
		parts = append(parts, r.Process)
	}
	kills := fmt.Sprintf("%d kills", r.Count)
	if r.Count == 1 {
		kills = "1 kill"
	}
	parts = append(parts, kills)
	switch {
	case cluster == nil:
		parts = append(parts, "container limit")
	case !found:
		parts = append(parts, "pod no longer exists")
	default:
		if c, ok := p.Containers[container]; ok {
			limit := c.Limit
			if limit == "" {
				limit = "none"
			}
			parts = append(parts, "limit "+limit)
		}
		if p.Owner != "" {
			parts = append(parts, p.Owner)
		}
	}
	return strings.Join(parts, " · ")
}
