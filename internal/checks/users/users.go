// Package users reports who is logged in, the recent logins, and root logins
// made today.
package users

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "users", Title: "Users & Logins", Order: 100}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("users", c.Meta().Title)
	r := env.Runner

	if who := lines(r.Run(ctx, "who").Stdout); len(who) > 0 {
		for _, l := range who {
			s.Add("Logged In", l, check.Info)
		}
	} else {
		s.Add("Logged In", "None", check.OK)
	}

	s.Separator("Recent Logins")
	recent := lines(r.Run(ctx, "last", "-n", "5").Stdout)
	for _, l := range recent[:min(5, len(recent))] {
		if strings.HasPrefix(l, "wtmp") || strings.HasPrefix(l, "reboot") {
			continue
		}
		s.Add("", trunc(l, 90), check.Info)
	}

	// `last root` returns history going back weeks. Marking all of it CAUTION
	// meant one root login in July kept the report yellow every day since.
	// Only a root session from today is current news.
	var root []string
	for _, l := range lines(r.Run(ctx, "last", "-n", "5", "root").Stdout) {
		if !strings.Contains(l, "wtmp") {
			root = append(root, l)
		}
	}
	if len(root) == 0 {
		return s
	}
	now := env.Now()
	today := 0
	s.Separator("Recent Root Logins")
	for _, l := range root {
		st := check.Info
		if loggedInOn(l, now) {
			st = check.Caution
			today++
		}
		s.Add("", trunc(l, 90), st)
	}
	if today > 0 {
		s.Alert(check.Caution, fmt.Sprintf("%d root login(s) today", today))
	}
	return s
}

// loggedInOn reports whether a `last` line is from now's date. `last` pads
// single-digit days with spaces ("Aug  5"), so it compares the month and day
// as separate fields rather than as text: that also keeps "Jan 15" from
// matching the 1st.
func loggedInOn(line string, now time.Time) bool {
	f := strings.Fields(line)
	mon := now.Format("Jan")
	for i := 0; i+1 < len(f); i++ {
		if f[i] != mon {
			continue
		}
		if d, err := strconv.Atoi(f[i+1]); err == nil && d == now.Day() {
			return true
		}
	}
	return false
}

// lines returns the non-empty lines of who or last output with each run of
// spaces collapsed to one. The column padding pushed a RHEL 7 `last` line
// with a remote host past the 78-column report, where it was cut off.
func lines(out string) []string {
	var res []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			res = append(res, strings.Join(f, " "))
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
