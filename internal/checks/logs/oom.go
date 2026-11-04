package logs

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/cri"
)

// The OOM Killer pattern counts kills, not lines: one kill writes about
// four lines ("invoked oom-killer", the oom_kill_process frame, the
// constraint or "Task in" line, "Killed process"). It also matches the two
// lines that say which cgroup was hit, which the rest of the pattern misses
// on the 3.10 kernel.
const (
	oomLabel   = "OOM Killer"
	oomPattern = `oom.kill|Out of memory|Killed process|Task in .* killed as a result of limit`
)

const (
	scopeContainer = "container limit" // the container hit its own memory limit
	scopeCgroup    = "cgroup limit"    // a limit outside Kubernetes (a systemd unit)
	scopeHost      = "host"            // the whole node ran out of memory
)

// killsListed is how many (pod, process) rows the section shows.
const killsListed = 5

var (
	oomLine = regexp.MustCompile(`(?i)` + oomPattern)
	// "Killed process 12345 (java) total-vm:…", alone (3.10) or after
	// "Out of memory: " / "Memory cgroup out of memory: ".
	killedLine = regexp.MustCompile(`Killed process (\d+) \(([^)]*)\)`)
	// 3.10 writes the scope one line before: "Memory cgroup out of memory:
	// Kill process 23456 (java) score 1990 or sacrifice child".
	sacrificeLine = regexp.MustCompile(`(?i)out of memory: Kill process \d+ \(`)
	// RHEL 8+: "oom-kill:constraint=CONSTRAINT_MEMCG,…,task_memcg=/kubepods…,task=java,pid=12345,uid=0".
	constraintLine = regexp.MustCompile(`oom-kill:constraint=(\w+)`)
	taskMemcg      = regexp.MustCompile(`task_memcg=([^,\s]+)`)
	taskName       = regexp.MustCompile(`[,\s]task=([^,\s]+)`)
	taskPid        = regexp.MustCompile(`[,\s]pid=(\d+)`)
	// 3.10: "Task in /kubepods/burstable/pod<uid>/<id> killed as a result of limit of …".
	taskInLine = regexp.MustCompile(`Task in (\S+) killed as a result of limit`)
	// A pod UID in a cgroup path; the systemd driver writes it with
	// underscores (kubepods-burstable-pod1a2b3c4d_5e6f_….slice).
	podUID = regexp.MustCompile(`pod([0-9a-f]{8}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{12})`)
	clock  = regexp.MustCompile(`\b(\d{2}):(\d{2}):(\d{2})\b`)
	// The date a line starts with: syslog's "Jan  5" or journalctl's
	// "Jan 05", or rsyslog's RFC 3339 "2026-01-05T".
	syslogDate = regexp.MustCompile(`^([A-Z][a-z]{2}) +(\d{1,2}) `)
	isoDate    = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})T`)
)

// kill is one OOM kill read from the kernel log.
type kill struct {
	at        time.Time // zero when the line has no time
	pid, proc string
	scope     string // one of the scope constants; "" when the log does not say
	uid, cid  string // pod UID and container ID, when in a container
}

// memcg is what a constraint or "Task in" line says about the next kill.
type memcg struct{ scope, uid, cid, task string }

func memcgOf(path string) memcg {
	m := memcg{scope: scopeCgroup, cid: cri.ContainerID(path)}
	if u := podUID.FindStringSubmatch(path); u != nil {
		m.uid = strings.ReplaceAll(u[1], "_", "-")
	}
	if m.uid != "" || m.cid != "" {
		m.scope = scopeContainer
	}
	return m
}

// todaysOOM keeps the lines that are OOM lines of today. grep already
// chose them; this drops what a looser grep or a log without dates let in.
func todaysOOM(lines []string, now time.Time) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		return !oomLine.MatchString(l) || !today(l, now)
	})
}

// today says whether a line is dated today; a line whose date it cannot
// read counts.
func today(l string, now time.Time) bool {
	if m := syslogDate.FindStringSubmatch(l); m != nil {
		d, _ := strconv.Atoi(m[2])
		return m[1] == now.Format("Jan") && d == now.Day()
	}
	if m := isoDate.FindStringSubmatch(l); m != nil {
		return m[1] == now.Format("2006-01-02")
	}
	return true
}

// at is the line's time of day on now's date.
func at(l string, now time.Time) time.Time {
	m := clock.FindStringSubmatch(l[:min(len(l), 40)])
	if m == nil {
		return time.Time{}
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	s, _ := strconv.Atoi(m[3])
	return time.Date(now.Year(), now.Month(), now.Day(), h, mi, s, 0, now.Location())
}

// parseKills reads the kills out of today's OOM lines, oldest first. A
// cgroup line pairs with the kill of the same pid when it names one,
// otherwise with the next kill.
func parseKills(lines []string, now time.Time) []kill {
	var kills []kill
	var next *memcg
	byPid := map[string]*memcg{}
	hint := ""
	for _, l := range lines {
		switch {
		case killedLine.MatchString(l):
			m := killedLine.FindStringSubmatch(l)
			k := kill{at: at(l, now), pid: m[1], proc: m[2]}
			c := byPid[k.pid]
			if c == nil {
				c = next
			}
			low := strings.ToLower(l)
			switch {
			case c != nil:
				k.scope = c.scope
			case strings.Contains(low, "memory cgroup out of memory"):
				k.scope = scopeCgroup
			case strings.Contains(low, "out of memory"):
				k.scope = scopeHost
			default:
				k.scope = hint
			}
			if c != nil {
				k.uid, k.cid = c.uid, c.cid
				if k.proc == "" {
					k.proc = c.task
				}
			}
			kills = append(kills, k)
			delete(byPid, k.pid)
			next, hint = nil, ""
		case constraintLine.MatchString(l):
			c := memcg{scope: scopeHost}
			if p := taskMemcg.FindStringSubmatch(l); p != nil {
				c = memcgOf(p[1])
			}
			if constraintLine.FindStringSubmatch(l)[1] != "CONSTRAINT_MEMCG" {
				c.scope = scopeHost
			}
			if t := taskName.FindStringSubmatch(l); t != nil {
				c.task = t[1]
			}
			if p := taskPid.FindStringSubmatch(l); p != nil {
				byPid[p[1]] = &c
			}
			next = &c
		case taskInLine.MatchString(l):
			c := memcgOf(taskInLine.FindStringSubmatch(l)[1])
			next = &c
		case sacrificeLine.MatchString(l):
			hint = scopeHost
			if strings.Contains(strings.ToLower(l), "memory cgroup") {
				hint = scopeCgroup
			}
		}
	}
	return kills
}

// killGroup is the kills of one process in one pod (or on the host).
type killGroup struct {
	kill
	n    int
	last time.Time
	ref  cri.Ref
	pod  bool // ref names the pod
}

// group folds the kills into one entry per (scope, process, pod or
// container): host kills first, then most kills, then the latest.
func group(kills []kill) []*killGroup {
	var out []*killGroup
	idx := map[string]*killGroup{}
	for _, k := range kills {
		key := k.scope + "|" + k.proc + "|" + k.uid + "|" + k.cid
		g := idx[key]
		if g == nil {
			g = &killGroup{kill: k}
			idx[key] = g
			out = append(out, g)
		}
		g.n++
		if k.at.After(g.last) {
			g.last = k.at
		}
	}
	slices.SortStableFunc(out, func(a, b *killGroup) int {
		if a.status() != b.status() {
			return int(b.status()) - int(a.status())
		}
		if a.n != b.n {
			return b.n - a.n
		}
		return b.last.Compare(a.last)
	})
	return out
}

// resolve names the pod of each group with crictl: by container ID first,
// which also gives the container's name, then by pod UID, which works
// after the killed container is gone. One call per distinct ID at most.
func resolve(ctx context.Context, env *check.Env, groups []*killGroup) {
	type answer struct {
		ref cri.Ref
		ok  bool
	}
	byCid, byUID := map[string]answer{}, map[string]answer{}
	for _, g := range groups {
		if g.cid != "" {
			a, done := byCid[g.cid]
			if !done {
				a.ref, a.ok = cri.Container(ctx, env.Runner, g.cid)
				byCid[g.cid] = a
			}
			if a.ok {
				g.ref, g.pod = a.ref, true
				continue
			}
		}
		if g.uid != "" {
			a, done := byUID[g.uid]
			if !done {
				a.ref, a.ok = cri.PodByUID(ctx, env.Runner, g.uid)
				byUID[g.uid] = a
			}
			g.ref, g.pod = a.ref, a.ok
		}
	}
}

func (g *killGroup) status() check.Status {
	if g.scope == scopeHost {
		return check.Unhealthy
	}
	return check.Caution
}

// text is "3× java · pod team-a/api-6b7c9d-x2k4p (container app) ·
// container limit · last 14:02".
func (g *killGroup) text() string {
	parts := []string{fmt.Sprintf("%d× %s", g.n, g.proc)}
	switch {
	case g.pod && g.ref.Container != "":
		parts = append(parts, fmt.Sprintf("pod %s (container %s)", g.ref.Pod(), g.ref.Container))
	case g.pod:
		parts = append(parts, "pod "+g.ref.Pod())
	case g.uid != "":
		parts = append(parts, "pod UID "+shortUID(g.uid)+" (not found)")
	case g.cid != "":
		parts = append(parts, "container "+g.cid[:12])
	}
	if g.scope != "" {
		parts = append(parts, g.scope)
	}
	if !g.last.IsZero() {
		parts = append(parts, "last "+g.last.Format("15:04"))
	}
	return strings.Join(parts, " · ")
}

// shortUID is the UID's first group: enough to grep for, short enough to
// read.
func shortUID(uid string) string {
	first, _, _ := strings.Cut(uid, "-")
	return first + "-…"
}

// oomRows reports today's kills: the count, the worst scope as the
// severity (a host out of memory is UNHEALTHY, a container at its limit
// CAUTION), one row per (pod, process), and the pods for the hub.
func oomRows(ctx context.Context, s *check.Section, env *check.Env, p Pattern, kills []kill) {
	n := len(kills)
	sev := p.Severity
	for _, k := range kills {
		if k.scope == scopeHost {
			sev = check.Unhealthy
		}
	}
	escalate := p.MinCount > 0 && n >= p.MinCount
	st := check.OK
	switch {
	case escalate:
		st = sev
	case n > 0:
		st = check.Info
	}
	s.Add(p.Label, fmt.Sprintf("%d kill(s) today", n), st, check.Detail(fmt.Sprintf("escalates at ≥ %d today", p.MinCount)))
	if !escalate {
		return
	}
	// Stable text, whatever pod was hit: the pods are in the rows and the
	// digest, and a new pod name every deploy must not read as a new alert.
	s.Alert(sev, fmt.Sprintf("%s: %d kill(s) today", p.Label, n))

	groups := group(kills)
	resolve(ctx, env, groups)
	s.Separator("Kills today")
	for i, g := range groups {
		if i == killsListed {
			s.Add("", fmt.Sprintf("… %d more", len(groups)-killsListed), check.Info)
			break
		}
		s.Add("", g.text(), g.status())
	}
	for _, g := range groups {
		if g.scope != scopeContainer || (g.uid == "" && !g.pod) {
			continue
		}
		ref := check.PodRef{UID: g.uid, Container: g.ref.Container, Process: g.proc, Reason: "oom_killed", Count: g.n, Last: g.last}
		if g.pod {
			ref.Namespace, ref.Name = g.ref.Namespace, g.ref.Name
		}
		s.Pods = append(s.Pods, ref)
	}
}
