// Package processes lists the heaviest processes and counts zombies.
package processes

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/runner"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle    `yaml:",inline"`
	ZombieCaution   int `yaml:"zombie_caution"`
	ZombieUnhealthy int `yaml:"zombie_unhealthy"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "processes", Title: "Top Processes", Order: 50}
}

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, ZombieCaution: 10, ZombieUnhealthy: 50}
}

// Validate keeps the thresholds in order.
func (c *Config) Validate() error {
	if c.ZombieCaution > c.ZombieUnhealthy {
		return fmt.Errorf("zombie_caution (%d) is above zombie_unhealthy (%d)", c.ZombieCaution, c.ZombieUnhealthy)
	}
	return nil
}

const (
	topN   = 5
	cmdMax = 40
)

// proc is one line of `ps aux`:
// USER PID %CPU %MEM VSZ RSS TTY STAT START TIME COMMAND.
type proc struct {
	pid      string
	cpu, mem string // as ps printed them, so the report shows ps's own figures
	cpuV     float64
	memV     float64
	stat     string
	cmd      string
}

// parsePS reads `ps aux`. The command is the remainder of the line after ten
// fields, spaces and all, cut to 40 characters.
func parsePS(out string) []proc {
	var ps []proc
	for _, line := range strings.Split(out, "\n") {
		f := splitN(line, 11)
		if len(f) < 11 || f[1] == "PID" {
			continue
		}
		cpu, err1 := strconv.ParseFloat(f[2], 64)
		mem, err2 := strconv.ParseFloat(f[3], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		cmd := []rune(strings.TrimSpace(f[10]))
		if len(cmd) > cmdMax {
			cmd = cmd[:cmdMax]
		}
		ps = append(ps, proc{pid: f[1], cpu: f[2], mem: f[3], cpuV: cpu, memV: mem, stat: f[7], cmd: string(cmd)})
	}
	return ps
}

// splitN splits on runs of whitespace into at most n fields, the last one
// holding the rest of the line untouched (Python's str.split(None, n-1)).
func splitN(s string, n int) []string {
	var out []string
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	for s != "" && len(out) < n-1 {
		i := strings.IndexFunc(s, unicode.IsSpace)
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = strings.TrimLeftFunc(s[i:], unicode.IsSpace)
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// top returns the n heaviest by key, keeping ps's order among equals.
func top(ps []proc, key func(proc) float64, n int) []proc {
	sorted := slices.Clone(ps)
	slices.SortStableFunc(sorted, func(a, b proc) int { return cmp.Compare(key(b), key(a)) })
	return sorted[:min(n, len(sorted))]
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("processes", c.Meta().Title)

	// One snapshot for both lists and the zombie count, sorted here rather
	// than by ps --sort, so the three always describe the same moment.
	res := env.Runner.Run(ctx, "ps", "aux")
	if !res.OK() && res.Stdout == "" {
		s.Add("Processes", "could not run ps: "+reason(res.Stderr, res.Err), check.Info)
		return s
	}
	ps := parsePS(res.Stdout)

	s.Separator("Top 5 by Memory")
	for _, p := range top(ps, func(p proc) float64 { return p.memV }, topN) {
		s.Add("PID "+p.pid, p.mem+"% MEM  "+p.cmd, check.Info)
	}
	s.Separator("Top 5 by CPU")
	for _, p := range top(ps, func(p proc) float64 { return p.cpuV }, topN) {
		s.Add("PID "+p.pid, p.cpu+"% CPU  "+p.cmd, check.Info)
	}

	// STAT starts with Z for a zombie; ps adds modifiers after it ("Z+",
	// "Zs"), which an exact match on "Z" used to miss.
	z := 0
	for _, p := range ps {
		if strings.HasPrefix(p.stat, "Z") {
			z++
		}
	}
	// A handful of zombies is routine on a busy host and clears itself; only
	// a pile that a human must go clean up is worth a notification.
	st := check.OK
	switch {
	case z >= cfg.ZombieUnhealthy:
		st = check.Unhealthy
	case z >= cfg.ZombieCaution:
		st = check.Caution
	case z > 0:
		st = check.Info
	}
	thresholds := fmt.Sprintf("Thresholds: caution ≥ %d, unhealthy ≥ %d", cfg.ZombieCaution, cfg.ZombieUnhealthy)
	if !st.Flagged() {
		s.Add("Zombie Processes", strconv.Itoa(z), st, check.Detail(thresholds))
		return s
	}
	// A count alone left a reader asking whether only a reboot clears them.
	// It never does: a zombie goes as soon as its parent exits, so name the
	// parents, and say so.
	parents := zombieParents(ctx, env.Runner)
	advice := "Restarting the parent clears them; no reboot needed."
	if slices.ContainsFunc(parents, func(p parent) bool { return p.containerInit }) {
		advice = "Restarting the parent's container clears them; no reboot needed. " +
			"A container's PID 1 must reap orphans: run it under tini, or set shareProcessNamespace: true."
	}
	s.Add("Zombie Processes", strconv.Itoa(z), st, check.Detail(advice+" "+thresholds))
	for _, p := range parents[:min(3, len(parents))] {
		what := p.comm
		if p.containerInit {
			what += " (container PID 1)"
		}
		s.Add("Parent PID "+p.pid, fmt.Sprintf("%d · %s", p.zombies, what), check.Info)
	}
	s.Alert(st, fmt.Sprintf("%d zombie processes", z))
	return s
}

// parent is a process holding zombies.
type parent struct {
	pid           string
	comm          string
	zombies       int
	containerInit bool
}

// zombieParents groups the zombies by parent, most first.
func zombieParents(ctx context.Context, r runner.Runner) []parent {
	count := map[string]int{}
	for _, line := range strings.Split(r.Run(ctx, "ps", "-e", "-o", "ppid=,stat=").Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[1], "Z") {
			count[f[0]]++
		}
	}
	var out []parent
	for pid, n := range count {
		p := parent{pid: pid, zombies: n, comm: "?"}
		if b, err := r.ReadFile("/proc/" + pid + "/comm"); err == nil {
			p.comm = strings.TrimSpace(string(b))
		}
		if b, err := r.ReadFile("/proc/" + pid + "/status"); err == nil {
			p.containerInit = nsInit(string(b))
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b parent) int {
		return cmp.Or(cmp.Compare(b.zombies, a.zombies), cmp.Compare(a.pid, b.pid))
	})
	return out
}

// nsInit reports a process that is PID 1 of a nested PID namespace: its
// NSpid line ("NSpid:\t3333254\t1") ends in 1. Kernels before 4.1 have no
// NSpid, and the host's own init has a single field.
func nsInit(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			f := strings.Fields(rest)
			return len(f) > 1 && f[len(f)-1] == "1"
		}
	}
	return false
}

func reason(stderr string, err error) string {
	if stderr != "" {
		return stderr
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}
