// Package crontabs reports cron entries added or removed since the previous
// run. A new line in a crontab is a classic persistence mechanism.
package crontabs

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/runner"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "crontabs", Title: "Crontab Changes", Order: 140}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

const stateKey = "crontabs"

// sources are files or directories; a directory contributes the regular
// files directly inside it (not its subdirectories, which is why
// /var/spool/cron/crontabs is listed on its own).
var sources = []string{
	"/etc/crontab",
	"/etc/cron.d",
	"/var/spool/cron",
	"/var/spool/cron/crontabs",
}

func (c Check) Run(_ context.Context, env *check.Env) *check.Section {
	s := check.NewSection("crontabs", c.Meta().Title)

	current := entries(env.Runner)
	var prev []string
	if env.State.Load(stateKey, &prev) {
		added, gone := diff(prev, current)
		if len(added) > 0 {
			s.Separator("New Crontab Entries")
			for _, l := range added {
				s.Add("NEW", truncate(l, 100), check.Caution)
			}
			s.Alert(check.Caution, "New crontab entry detected")
		}
		if len(gone) > 0 {
			s.Separator("Removed Crontab Entries")
			for _, l := range gone {
				s.Add("GONE", truncate(l, 100), check.Info)
			}
		}
		if len(added) == 0 && len(gone) == 0 {
			s.Add("Crontab Changes", fmt.Sprintf("None (%d entries)", len(current)), check.OK)
		}
	} else {
		s.Add("Baseline", fmt.Sprintf("%d crontab entries recorded (first run)", len(current)), check.Info)
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving crontab baseline", "err", err)
	}
	return s
}

// entries returns every active cron line as "<file>: <line>", sorted and
// de-duplicated. Unreadable sources are skipped, as a non-root run must
// still report what it can see.
func entries(r runner.Runner) []string {
	var files []string
	for _, src := range sources {
		list, err := r.ReadDir(src)
		if err != nil {
			// Not a directory: the source is a file, or absent.
			files = append(files, src)
			continue
		}
		for _, e := range list {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				files = append(files, path.Join(src, e.Name()))
			}
		}
	}
	var lines []string
	for _, f := range files {
		b, err := r.ReadFile(f)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			lines = append(lines, f+": "+l)
		}
	}
	slices.Sort(lines)
	return slices.Compact(lines)
}

// truncate shortens to n characters, not bytes, so a multi-byte character
// is never cut in half.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
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
