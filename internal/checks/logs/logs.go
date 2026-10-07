// Package logs counts today's occurrences of known-bad patterns in the
// system and authentication logs.
package logs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checks/logscan"
)

func init() { check.Register(Check{}) }

// Pattern is one thing to look for in today's logs.
type Pattern struct {
	Label string `yaml:"label"`
	// Pattern is a case-insensitive extended regex (grep -iE).
	Pattern string `yaml:"pattern"`
	// Source is "system" (syslog, messages, kern.log, dmesg) or "auth". With
	// journalctl the whole journal is searched either way.
	Source   string       `yaml:"source"`
	Severity check.Status `yaml:"severity"`
	// MinCount is how many occurrences today escalate to Severity; 0 means
	// count only, never escalate.
	MinCount int `yaml:"min_count"`
}

type Config struct {
	check.Toggle `yaml:",inline"`
	// "Invalid user" / "BREAK-IN ATTEMPT" lines today before SSH probing is
	// worth a CAUTION. Internet-facing hosts see a constant background level.
	SSHBruteForceMin int `yaml:"ssh_brute_force_min"`
	// Site-specific patterns, checked after the built-in ones.
	ExtraPatterns []Pattern `yaml:"extra_patterns"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "logs", Title: "Log Analysis (today)", Order: 180}
}

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, SSHBruteForceMin: 50, ExtraPatterns: []Pattern{}}
}

const (
	system = "system"
	auth   = "auth"
)

// builtin is the pattern table. MinCount exists because a single segfault of
// a desktop helper is not an incident, while a single kernel panic obviously
// is.
func builtin(sshMin int) []Pattern {
	return []Pattern{
		{"OOM Killer", `oom.kill|Out of memory`, system, check.Caution, 1},
		{"Disk I/O Errors", `I/O error|blk_update_request`, system, check.Caution, 1},
		{"Kernel Panic", `kernel panic`, system, check.Unhealthy, 1},
		{"Segmentation Faults", `segfault|SIGSEGV`, system, check.Caution, 10},
		{"CPU Machine Check", `machine check|MCE.*Bank`, system, check.Caution, 1},
		{"Filesystem Errors", `EXT[234]-fs error|XFS.*error`, system, check.Caution, 1},
		{"SSH Brute Force", `BREAK-IN ATTEMPT|Invalid user`, auth, check.Caution, sshMin},
		{"USB Device Added", `New USB device found`, system, check.Info, 0},
		{"sudo Escalation", `sudo.*COMMAND`, auth, check.Info, 0},
	}
}

// Validate checks the escalation counts and the extra patterns.
func (c *Config) Validate() error {
	var errs []error
	if c.SSHBruteForceMin < 0 || c.SSHBruteForceMin > logscan.MaxLines {
		// The scan keeps only the last MaxLines matches, so a larger
		// threshold could never be reached.
		errs = append(errs, fmt.Errorf("ssh_brute_force_min (%d) must be between 0 and %d", c.SSHBruteForceMin, logscan.MaxLines))
	}
	seen := map[string]bool{}
	for _, p := range builtin(0) {
		seen[p.Label] = true
	}
	for i, p := range c.ExtraPatterns {
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("extra_patterns[%d]: "+format, append([]any{i}, a...)...))
		}
		switch {
		case p.Label == "":
			bad("label is empty")
		case seen[p.Label]:
			bad("label %q is already used", p.Label)
		}
		seen[p.Label] = true
		if p.Pattern == "" {
			bad("pattern is empty")
		}
		if !slices.Contains([]string{"", system, auth}, p.Source) {
			bad("source %q is not one of system, auth", p.Source)
		}
		if p.Severity < check.Info {
			bad("severity must be one of info, caution, unhealthy")
		}
		if p.MinCount < 0 || p.MinCount > logscan.MaxLines {
			bad("min_count (%d) must be between 0 and %d", p.MinCount, logscan.MaxLines)
		}
	}
	return errors.Join(errs...)
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("logs", c.Meta().Title)
	r := env.Runner
	now := env.Now()
	sources := map[string][]string{system: logscan.SystemLogs(r)}
	if a := logscan.AuthLog(r); a != "" {
		sources[auth] = []string{a}
	}
	_, journal := r.LookPath("journalctl")

	scanned := false
	for _, p := range append(builtin(cfg.SSHBruteForceMin), cfg.ExtraPatterns...) {
		src := p.Source
		if src == "" {
			src = system
		}
		if !journal && len(sources[src]) == 0 {
			continue
		}
		scanned = true
		count, samples := logscan.Today(ctx, r, now, sources[src], p.Pattern)

		severe := p.Severity >= check.Caution && p.MinCount > 0
		escalate := severe && count >= p.MinCount
		st := check.OK
		switch {
		case escalate:
			st = p.Severity
		case count > 0:
			st = check.Info
		}
		var opts []check.RowOpt
		if severe {
			opts = append(opts, check.Detail(fmt.Sprintf("escalates at ≥ %d today", p.MinCount)))
		}
		s.Add(p.Label, fmt.Sprintf("%d occurrence(s) today", count), st, opts...)

		if escalate {
			s.Alert(p.Severity, fmt.Sprintf("%s: %d occurrence(s) today", p.Label, count))
			if len(samples) > 0 {
				s.Separator("Recent entries")
				for _, l := range samples {
					s.Add("", trunc(l, 120), p.Severity)
				}
			}
		}
	}
	if !scanned {
		s.Add("Log Sources", "None found (no journalctl, /var/log/syslog, /var/log/messages or auth log)", check.Info)
	}
	return s
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
