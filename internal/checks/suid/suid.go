// Package suid reports set-user-ID files added or removed since the previous
// run. A new SUID binary is how a foothold becomes root.
package suid

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/runner"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "suid", Title: "SUID Files", Order: 150}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

const (
	stateKey = "suid_files"
	timeout  = 120 * time.Second
)

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("suid", c.Meta().Title)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// -perm -4000 rather than /4000: older findutils (RHEL 6 era) do not
	// know the slash form, and for a single bit the two mean the same.
	// A non-zero exit is normal (permission denied somewhere under /); only
	// a failure to run or a timeout means the list is incomplete.
	res := env.Runner.Run(ctx, "find", "/", "-xdev", "-perm", "-4000", "-type", "f")
	if res.Code < 0 || res.Err != nil {
		// Saving a partial list would report every missing file as new on
		// the next run, so keep the previous baseline untouched.
		s.Add("SUID Scan", "Did not complete", check.Caution,
			check.Detail(fmt.Sprintf("%s; baseline kept, retried next run", why(res))))
		return s
	}
	var current []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			current = append(current, l)
		}
	}
	slices.Sort(current)
	current = slices.Compact(current)

	var prev []string
	if env.State.Load(stateKey, &prev) {
		added, gone := diff(prev, current)
		if len(added) > 0 {
			s.Separator("New SUID Files")
			for _, f := range added {
				s.Add("NEW", f, check.Unhealthy)
				s.Alert(check.Unhealthy, "New SUID file: "+f)
			}
		}
		if len(gone) > 0 {
			s.Separator("Removed SUID Files")
			for _, f := range gone {
				s.Add("GONE", f, check.Info)
			}
		}
		if len(added) == 0 && len(gone) == 0 {
			s.Add("SUID Changes", fmt.Sprintf("None (%d files, unchanged)", len(current)), check.OK)
		}
	} else {
		s.Add("Baseline", fmt.Sprintf("%d SUID files recorded (first run)", len(current)), check.Info)
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving SUID baseline", "err", err)
	}
	return s
}

func why(r runner.Result) string {
	switch {
	case r.Err != nil:
		return r.Err.Error()
	case r.Stderr != "":
		return r.Stderr
	}
	return "find failed"
}

// diff returns what is in cur but not prev, and what is in prev but not cur.
func diff(prev, cur []string) (added, gone []string) {
	p, c := set(prev), set(cur)
	for _, v := range cur {
		if !p[v] {
			added = append(added, v)
		}
	}
	for _, v := range prev {
		if !c[v] {
			gone = append(gone, v)
		}
	}
	return added, gone
}

func set(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, v := range list {
		m[v] = true
	}
	return m
}
