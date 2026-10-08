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
	s.Add("Zombie Processes", strconv.Itoa(z), st,
		check.Detail(fmt.Sprintf("Thresholds: caution ≥ %d, unhealthy ≥ %d", cfg.ZombieCaution, cfg.ZombieUnhealthy)))
	if st.Flagged() {
		s.Alert(st, fmt.Sprintf("%d zombie processes", z))
	}
	return s
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
