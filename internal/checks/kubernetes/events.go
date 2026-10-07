package kubernetes

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
)

// Warning events that describe the NODE running out of something. Everything
// else (Unhealthy probe blips, BackOff, FailedScheduling) is routine on a
// busy cluster at any volume, and is reported as a count only.
var seriousEvents = []string{
	"SystemOOM", "OOMKilling", "NodeHasInsufficientMemory", "NodeHasDiskPressure",
	"NodeHasInsufficientPID", "FreeDiskSpaceFailed", "ImageGCFailed",
	"ContainerGCFailed", "EvictionThresholdMet", "Evicted", "NodeNotReady",
	"InvalidDiskCapacity", "KubeletSetupFailed", "HostPortConflict",
	"FailedAttachVolume",
}

const eventColumns = "custom-columns=REASON:.reason,COUNT:.count,LAST:.lastTimestamp"

type reasonCount struct {
	reason string
	n      int
}

func joinCounts(rs []reasonCount, sep string) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("%s ×%d", r.reason, r.n)
	}
	return strings.Join(parts, sep)
}

func eventRows(ctx context.Context, s *check.Section, env *check.Env, k kube) {
	// `kubectl get events`, not `kubectl events`: the latter only reached GA
	// in 1.26 and reports a different API with different columns.
	res := k.run(ctx, "get", "events", "--all-namespaces", "--field-selector", "type=Warning",
		"--no-headers", "-o", eventColumns)
	out := combined(res)
	if !res.OK() || out == "" || strings.Contains(strings.ToLower(out), "no resources found") {
		return
	}

	var ranked []reasonCount // first-seen order, so equal counts keep it
	idx := map[string]int{}
	var oldest time.Time
	total := 0
	for _, line := range strings.Split(res.Stdout, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		reason := parts[0]
		// '<none>' on events written through events.k8s.io/v1, which carry
		// eventTime instead of count.
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			n = 1
		}
		if i, ok := idx[reason]; ok {
			ranked[i].n += n
		} else {
			idx[reason] = len(ranked)
			ranked = append(ranked, reasonCount{reason, n})
		}
		total += n
		if len(parts) > 2 && parts[2] != "<none>" {
			if ts, err := time.Parse("2006-01-02T15:04:05Z", parts[2]); err == nil && (oldest.IsZero() || ts.Before(oldest)) {
				oldest = ts
			}
		}
	}
	if len(ranked) == 0 {
		return
	}

	// NOT "last 24h": the apiserver's --event-ttl defaults to one hour and
	// both k3s and rke2 ship that default, so the only honest window is the
	// one the data itself describes.
	window := "retained window"
	if !oldest.IsZero() {
		if mins := env.Now().UTC().Sub(oldest).Minutes(); mins >= 1 {
			window = fmt.Sprintf("~%.0fm retained", mins)
		} else {
			window = "just now"
		}
	}

	slices.SortStableFunc(ranked, func(a, b reasonCount) int { return cmp.Compare(b.n, a.n) })
	s.Add("Warning Events", fmt.Sprintf("%d in the %s: %s", total, window, joinCounts(firstN(ranked, 5), ", ")), check.Info)

	var serious []reasonCount
	for _, r := range ranked {
		if slices.Contains(seriousEvents, r.reason) {
			serious = append(serious, r)
		}
	}
	if len(serious) > 0 {
		s.Add("Node-level Events", joinCounts(firstN(serious, 5), " · "), check.Caution)
		s.Alert(check.Caution, "Kubernetes node events: "+joinCounts(firstN(serious, 3), ", "))
	}
}
