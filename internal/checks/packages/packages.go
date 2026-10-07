// Package packages reports packages installed or removed since the previous
// run, as an audit trail.
package packages

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
	return check.Meta{Name: "packages", Title: "Package Changes", Order: 160}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

const (
	stateKey = "packages"
	listCap  = 20
)

// rpmQueryFormat is handed to rpm as one argument; rpm itself expands \n.
const rpmQueryFormat = `%{NAME}-%{VERSION}-%{RELEASE}\n`

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("packages", c.Meta().Title)
	r := env.Runner
	pm := env.Host.PkgManager

	var current []string
	switch pm {
	case "dnf", "yum", "zypper":
		for _, l := range strings.Split(r.Run(ctx, "rpm", "-qa", "--qf", rpmQueryFormat).Stdout, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				current = append(current, l)
			}
		}
	case "apt-get":
		// dpkg -l does not truncate its columns when stdout is not a tty.
		for _, l := range strings.Split(r.Run(ctx, "dpkg", "-l").Stdout, "\n") {
			if f := strings.Fields(l); len(f) >= 3 && f[0] == "ii" {
				current = append(current, f[1]+"-"+f[2])
			}
		}
	default:
		s.Add("Package Manager", "Not detected", check.Info)
		return s
	}
	slices.Sort(current)
	current = slices.Compact(current)

	// Every host has packages. An empty list means the query failed, and
	// saving it would report the whole system as freshly installed tomorrow.
	if len(current) == 0 {
		s.Add("Installed Packages", "Could not be listed", check.Caution,
			check.Detail("the package database query returned nothing; baseline kept"))
		return s
	}

	var prev []string
	if env.State.Load(stateKey, &prev) {
		added, gone := diff(prev, current)
		// Package churn is expected on any host with unattended upgrades.
		// Keep it in the report as an audit trail, but do not page anyone.
		listed(s, "Newly Installed", "INSTALLED", added)
		listed(s, "Removed", "REMOVED", gone)
		if len(added) == 0 && len(gone) == 0 {
			s.Add("Package Changes", fmt.Sprintf("None (%d packages)", len(current)), check.OK)
		}
	} else {
		s.Add("Baseline", fmt.Sprintf("%d packages recorded (first run)", len(current)), check.Info)
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving package baseline", "err", err)
	}

	// zypper keeps no numbered transaction history to show here.
	if pm == "dnf" || pm == "yum" {
		var hist []string
		for i, l := range strings.Split(r.Run(ctx, pm, "history", "list").Stdout, "\n") {
			if i == 5 {
				break
			}
			if strings.TrimSpace(l) != "" {
				hist = append(hist, truncate(l, 100))
			}
		}
		if len(hist) > 0 {
			s.Separator("Recent Transaction History")
			for _, l := range hist {
				s.Add("", l, check.Info)
			}
		}
	}
	return s
}

func listed(s *check.Section, title, label string, pkgs []string) {
	if len(pkgs) == 0 {
		return
	}
	s.Separator(title)
	for i, p := range pkgs {
		if i == listCap {
			s.Add("...", fmt.Sprintf("and %d more", len(pkgs)-listCap), check.Info)
			break
		}
		s.Add(label, p, check.Info)
	}
}

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
