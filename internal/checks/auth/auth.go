// Package auth reports today's SSH failures, successful logins and sudo use.
package auth

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checks/logscan"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	// Failed SSH attempts today. Internet-facing hosts see a constant
	// background level, so these are deliberately high.
	FailedSSHCaution   int `yaml:"failed_ssh_caution"`
	FailedSSHUnhealthy int `yaml:"failed_ssh_unhealthy"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "auth", Title: "SSH & Authentication", Order: 110}
}

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, FailedSSHCaution: 50, FailedSSHUnhealthy: 500}
}

// Validate keeps the thresholds in order.
func (c *Config) Validate() error {
	if c.FailedSSHCaution < 0 {
		return fmt.Errorf("failed_ssh_caution (%d) must not be negative", c.FailedSSHCaution)
	}
	if c.FailedSSHCaution > c.FailedSSHUnhealthy {
		return fmt.Errorf("failed_ssh_caution (%d) is above failed_ssh_unhealthy (%d)", c.FailedSSHCaution, c.FailedSSHUnhealthy)
	}
	return nil
}

const (
	failedPattern = "Failed password"
	okPattern     = `Accepted (password|publickey)`
	sudoPattern   = `sudo.*COMMAND`
)

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("auth", c.Meta().Title)
	r := env.Runner
	now := env.Now()
	authLog := logscan.AuthLog(r)
	thrC, thrU := cfg.FailedSSHCaution, cfg.FailedSSHUnhealthy

	nFailed := logscan.Count(ctx, r, now, authLog, failedPattern)
	st := check.OK
	switch {
	case nFailed >= thrU:
		st = check.Unhealthy
	case nFailed >= thrC:
		st = check.Caution
	case nFailed > 0:
		st = check.Info
	}
	s.Add("Failed SSH Attempts (today)", strconv.Itoa(nFailed), st,
		check.Detail(fmt.Sprintf("Thresholds: caution ≥ %d, unhealthy ≥ %d", thrC, thrU)))
	if st.Flagged() {
		s.Alert(st, fmt.Sprintf("%d failed SSH attempts today", nFailed))
	}

	if authLog != "" && nFailed > 0 {
		// The last 1000 failures are plenty to name the loudest sources;
		// grep streams the file so a flooded log is never read into memory.
		out := r.Shell(ctx, "grep -F "+logscan.Quote(failedPattern)+" "+logscan.Quote(authLog)+" 2>/dev/null | tail -1000").Stdout
		if top := topIPs(out, 5); len(top) > 0 {
			s.Separator("Top Attacking IPs")
			for _, ip := range top {
				st := check.Info
				if ip.n >= thrC {
					st = check.Caution
				}
				s.Add(ip.addr, fmt.Sprintf("%d attempts", ip.n), st)
			}
		}
	}

	s.Add("Successful SSH Logins (today)", strconv.Itoa(logscan.Count(ctx, r, now, authLog, okPattern)), check.Info)

	nSudo := logscan.Count(ctx, r, now, authLog, sudoPattern)
	s.Add("Sudo Commands (today)", strconv.Itoa(nSudo), check.Info)
	if nSudo > 0 && authLog != "" {
		out := r.Shell(ctx, "grep -E "+logscan.Quote(sudoPattern)+" "+logscan.Quote(authLog)+
			" 2>/dev/null | grep -E "+logscan.Quote(logscan.TodayRE(now))+" | tail -5").Stdout
		if recent := nonBlank(out); len(recent) > 0 {
			s.Separator("Recent sudo Commands")
			for _, l := range recent {
				s.Add("", trunc(l, 100), check.Info)
			}
		}
	}

	if authLog == "" {
		// Not a fault on journald-only systems, where journalctl is the source.
		if _, ok := r.LookPath("journalctl"); ok {
			s.Add("Auth Log", "Not found — using journalctl", check.Info)
		} else {
			s.Add("Auth Log", "Not found (/var/log/auth.log or /var/log/secure)", check.Caution)
		}
	}
	return s
}

var fromIP = regexp.MustCompile(`from ([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)`)

type source struct {
	addr string
	n    int
}

// topIPs counts the IPv4 sources of "Failed password" lines, most frequent
// first, at most limit of them.
func topIPs(out string, limit int) []source {
	counts := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		for _, m := range fromIP.FindAllStringSubmatch(l, -1) {
			counts[m[1]]++
		}
	}
	res := make([]source, 0, len(counts))
	for a, n := range counts {
		res = append(res, source{a, n})
	}
	slices.SortFunc(res, func(a, b source) int {
		return cmp.Or(cmp.Compare(b.n, a.n), strings.Compare(a.addr, b.addr))
	})
	return res[:min(limit, len(res))]
}

func nonBlank(out string) []string {
	var res []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			res = append(res, l)
		}
	}
	return res
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
