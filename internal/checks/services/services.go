// Package services reports failed systemd units and units stuck starting.
package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thyarles/lhc-go/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "services", Title: "System Services", Order: 60}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

const (
	notSystemd    = "systemd not available on this system"
	activatingKey = "activating"
	activatingCap = 5
)

// noSystemd recognises a systemctl that exists but has no systemd to talk
// to: containers, WSL without systemd, chroots. That is an absent subsystem,
// not a failure. "Failed to connect to bus" alone could also be a broken
// D-Bus on a real systemd host, so it counts only when /run/systemd/system,
// systemd's own "I am PID 1" marker, is missing too.
func noSystemd(out string, booted bool) bool {
	return strings.Contains(out, "not been booted with systemd") ||
		(!booted && strings.Contains(out, "Failed to connect to bus"))
}

// units splits `systemctl list-units --no-legend` output into fields,
// dropping the "●"/"*" marker newer systemd puts before a failed unit.
func units(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "●"), "* "))
		if line == "" {
			continue
		}
		rows = append(rows, splitN(line, 5))
	}
	return rows
}

// splitN splits on runs of whitespace into at most n fields, the last one
// keeping the rest of the line (a unit description has spaces).
func splitN(s string, n int) []string {
	var out []string
	s = strings.TrimSpace(s)
	for s != "" && len(out) < n-1 {
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = strings.TrimLeft(s[i:], " \t")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("services", c.Meta().Title)
	r := env.Runner

	if _, ok := r.LookPath("systemctl"); !ok {
		s.NotApplicable(notSystemd)
		return s
	}

	res := r.Run(ctx, "systemctl", "list-units", "--state=failed", "--no-legend", "--no-pager")
	if !res.OK() {
		if noSystemd(res.Stderr+"\n"+res.Stdout, r.Exists("/run/systemd/system")) {
			s.NotApplicable(notSystemd)
			return s
		}
		// Anything else is not "no failed units": say we could not tell,
		// without paging anyone over it.
		msg := res.Stderr
		if msg == "" && res.Err != nil {
			msg = res.Err.Error()
		}
		s.Add("Failed Services", "could not query systemd: "+msg, check.Info)
		return s
	}

	failed := units(res.Stdout)
	for _, p := range failed {
		unit, desc := p[0], "failed"
		if len(p) > 4 {
			desc = p[4]
		}
		s.Add(unit, desc, check.Unhealthy)
		s.Alert(check.Unhealthy, fmt.Sprintf("Service %s failed", unit))
	}
	if len(failed) == 0 {
		s.Add("Failed Services", "None", check.OK)
	}

	// A unit caught mid-start is normal — the report just happened to run
	// during its startup. It is only "stuck" if it was still activating on
	// the previous run too, so compare against the last snapshot.
	out := r.Run(ctx, "systemctl", "list-units", "--state=activating", "--no-legend", "--no-pager").Stdout
	activating := []string{}
	for _, p := range units(out) {
		if len(activating) == activatingCap {
			break
		}
		activating = append(activating, p[0])
	}
	var before []string
	env.State.Load(activatingKey, &before)
	for _, unit := range activating {
		if slices.Contains(before, unit) {
			s.Add(unit, "still activating since the previous run", check.Caution)
			s.Alert(check.Caution, fmt.Sprintf("Service %s stuck activating", unit))
		} else {
			s.Add(unit, "activating (starting up)", check.Info)
		}
	}
	if err := env.State.Save(activatingKey, activating); err != nil {
		env.Log.Warn("saving activating units", "err", err)
	}
	return s
}
