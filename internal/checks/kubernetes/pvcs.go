package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

// custom-columns, never the table: kubectl 1.31 inserted
// VOLUMEATTRIBUTESCLASS before AGE, and the ACCESS MODES header has a space.
const pvcColumns = "custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,SC:.spec.storageClassName"

func pvcRows(ctx context.Context, s *check.Section, env *check.Env, k kube) {
	res := k.run(ctx, "get", "pvc", "--all-namespaces", "--no-headers", "-o", pvcColumns)
	out := combined(res)
	if !res.OK() || out == "" || strings.Contains(strings.ToLower(out), "no resources found") {
		return
	}

	var prevPending []string
	env.State.Load("pending_pvcs", &prevPending)
	pending := []string{}
	var lost []string
	bound := 0

	for _, line := range strings.Split(res.Stdout, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		key, phase := parts[0]+"/"+parts[1], parts[2]
		switch phase {
		case "Bound":
			bound++
		case "Lost":
			lost = append(lost, key)
			s.Add(key, "Lost — backing volume is gone", check.Unhealthy)
		case "Pending":
			pending = append(pending, key)
			class := "no class"
			if len(parts) > 3 {
				class = parts[3]
			}
			// local-path and most cloud storage classes are
			// WaitForFirstConsumer, so a claim with no consumer is Pending
			// forever by design. Only one still pending next run is news.
			if slices.Contains(prevPending, key) {
				s.Add(key, fmt.Sprintf("Pending (%s)", class), check.Caution,
					check.Detail("pending since the last run too"))
			} else {
				s.Add(key, fmt.Sprintf("Pending (%s)", class), check.Info,
					check.Detail("may be WaitForFirstConsumer — checked again next run"))
			}
		}
	}

	save(env, "pending_pvcs", pending)

	if bound > 0 || len(pending) > 0 || len(lost) > 0 {
		s.Add("PersistentVolumeClaims", fmt.Sprintf("%d bound", bound), check.Info)
	}
	if len(lost) > 0 {
		s.Alert(check.Unhealthy, "Kubernetes PVC lost: "+strings.Join(firstN(lost, 3), ", "))
	}
	var still []string
	for _, p := range pending {
		if slices.Contains(prevPending, p) {
			still = append(still, p)
		}
	}
	if len(still) > 0 {
		s.Alert(check.Caution, "Kubernetes PVC still Pending: "+strings.Join(firstN(still, 3), ", "))
	}
}
