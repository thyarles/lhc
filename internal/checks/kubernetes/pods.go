package kubernetes

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

// kubectl's AGE column, produced by apimachinery's HumanDuration: 45s, 5m30s,
// 3h20m, 5d3h, 2y40d. No weeks and no months, unlike Docker's humaniser.
var ageRE = regexp.MustCompile(`(\d+)([smhdy])`)

var ageUnits = map[string]float64{"s": 1, "m": 60, "h": 3600, "d": 86400, "y": 31536000}

// ageSeconds reads a kubectl AGE cell; -1 when it cannot be read.
func ageSeconds(age string) float64 {
	var total float64
	for _, m := range ageRE.FindAllStringSubmatch(age, -1) {
		q, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			total += q * ageUnits[m[2]]
		}
	}
	if total == 0 {
		return -1 // '<unknown>': do not escalate on age
	}
	return total
}

type pod struct {
	ns, name        string
	ready           string
	readyN, readyOf int
	status          string
	restarts        int
	since           string // display only: absent before kubectl 1.23
	ageS            float64
	age             string
}

// parsePodLine reads NAMESPACE NAME READY STATUS RESTARTS AGE.
//
// RESTARTS is '12' before kubectl 1.23 and '12 (5m ago)' after, so the line
// has six tokens or eight, never seven. Anchor on both ends (restarts at
// index 4, AGE always last), never on a token count. Namespace and pod names
// are RFC 1123 labels, so they can never contain whitespace.
func parsePodLine(line string) (pod, bool) {
	p := strings.Fields(line)
	if len(p) < 6 {
		return pod{}, false
	}
	rN, rOf, _ := strings.Cut(p[2], "/")
	out := pod{
		ns: p[0], name: p[1], ready: p[2], status: p[3],
		readyN: atoiOrZero(rN), readyOf: atoiOrZero(rOf),
		restarts: atoiOrZero(p[4]),
		ageS:     ageSeconds(p[len(p)-1]),
		age:      p[len(p)-1],
	}
	if len(p) >= 8 && strings.HasPrefix(p[5], "(") {
		out.since = strings.Trim(strings.Join(p[5:len(p)-1], " "), "()")
	}
	return out, true
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

var (
	brokenReasons = []string{
		"crashloopbackoff", "imagepullbackoff", "errimagepull", "error",
		"createcontainerconfigerror", "createcontainererror", "invalidimagename",
		"runcontainererror", "oomkilled",
	}
	settling = []string{"pending", "containercreating", "podinitializing", "terminating"}
	done     = []string{"completed", "succeeded"}
)

func podRows(ctx context.Context, s *check.Section, env *check.Env, cfg *Config, k kube, me *node) {
	args := []string{"get", "pods", "--all-namespaces", "--no-headers"}
	if me != nil {
		args = append(args, "--field-selector=spec.nodeName="+me.name)
	}
	res := k.run(ctx, args...)
	if !res.OK() {
		s.Add("Pods", clip(firstLine(failure(res, "query failed")), 80), check.Info)
		return
	}
	if strings.Contains(strings.ToLower(combined(res)), "no resources found") {
		return
	}

	pendS := cfg.PendingMinutes * 60
	evictS := cfg.EvictedRecentHours * 3600

	prevR := map[string]int{}
	env.State.Load("pod_restarts", &prevR)
	var prevDeg []string
	env.State.Load("degraded_pods", &prevDeg)

	nowR, degraded := map[string]int{}, []string{}
	var problems []string
	running, completed, quiet := 0, 0, 0

	lines := strings.Split(res.Stdout, "\n")
	lines = lines[:min(len(lines), cfg.MaxPods)]
	for _, line := range lines {
		p, ok := parsePodLine(line)
		if !ok {
			continue
		}
		key := p.ns + "/" + p.name
		low := strings.ToLower(p.status)
		nowR[key] = p.restarts

		// The restart COUNT is meaningless: 200 days up with 5 restarts is a
		// healthy pod. The GROWTH since the last run is the signal. Keyed on
		// ns/name, so a rolling deploy's new pod hash starts from zero.
		prev, seen := prevR[key]
		if !seen {
			prev = p.restarts
		}
		note := ""
		if grew := p.restarts - prev; grew >= cfg.RestartDeltaCaution {
			note = fmt.Sprintf("+%d restart(s) since last run", grew)
		}

		var st check.Status
		switch {
		case slices.Contains(done, low):
			// helm-install-*, svclb-* and CronJob pods sit Completed for
			// months: the biggest source of Kubernetes noise, and the
			// analogue of docker's Exited (0) -> INFO.
			completed++
			st = check.Info
		case containsAny(low, brokenReasons):
			st = check.Caution
		case low == "evicted":
			// Evicted pod objects persist until GC. An eviction from last
			// month is not today's news.
			st = check.Info
			if p.ageS >= 0 && p.ageS <= evictS {
				st = check.Caution
			}
		case strings.HasPrefix(low, "init:") || slices.Contains(settling, low):
			st = check.Info
			if p.ageS > pendS {
				st = check.Caution
			}
		case low == "running" && p.readyOf != 0 && p.readyN < p.readyOf:
			// A pod that restarted thirty seconds before the cron is 0/1 for
			// a moment. Only a pod still degraded on the next run is news.
			degraded = append(degraded, key)
			st = check.Info
			if slices.Contains(prevDeg, key) {
				st = check.Caution
			}
		case low == "running":
			running++
			st = check.OK
		default:
			st = check.Info // unknown printer strings must not invent incidents
		}

		// A pod that looks fine but picked up restarts since the last run is
		// not fine. Applied after classification, or a Running pod would be
		// dismissed as OK before the growth is considered.
		if note != "" {
			st = check.Caution
		}

		if st != check.Caution {
			if st == check.Info && !slices.Contains(done, low) {
				quiet++
			}
			continue
		}

		problems = append(problems, key)
		if len(problems) <= listCap {
			var details []string
			if note != "" {
				details = append(details, note)
			}
			if p.since != "" {
				details = append(details, "last restart "+p.since)
			}
			value := fmt.Sprintf("%s %s · %s", p.ready, p.status, p.age)
			if p.restarts != 0 {
				value += fmt.Sprintf(" · %d restart(s)", p.restarts)
			}
			s.Add(key, value, st, check.Detail(strings.Join(details, "; ")))
		}
	}

	save(env, "pod_restarts", nowR)
	save(env, "degraded_pods", degraded)

	scopeLabel := "cluster-wide"
	if me != nil {
		scopeLabel = "on this host"
	}
	// Healthy pods are a count, never a wall of names. 'settling' keeps young
	// Pending/ContainerCreating pods visible in the total.
	value := fmt.Sprintf("%d running · %d completed", running, completed)
	if quiet > 0 {
		value += fmt.Sprintf(" · %d settling", quiet)
	}
	value += fmt.Sprintf(" · %d needing attention", len(problems))
	s.Add(fmt.Sprintf("Pods (%s)", scopeLabel), value, check.Info)
	if len(problems) > listCap {
		s.Add("…", fmt.Sprintf("and %d more pod(s) needing attention", len(problems)-listCap), check.Info)
	}
	if len(problems) > 0 {
		s.Alert(check.Caution, "Kubernetes pods: "+strings.Join(firstN(problems, 3), ", ")+moreSuffix(len(problems)))
	}
}
