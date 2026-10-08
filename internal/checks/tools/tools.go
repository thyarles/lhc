// Package tools reports which of the commands lhc relies on are installed.
// Tools is also what `lhc tools --install` installs.
package tools

import (
	"context"

	"github.com/thyarles/lhc/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "tools", Title: "System Tools Status", Order: 210}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

// Tools are the commands some check uses. Required ones reduce coverage of
// checks every host runs; optional ones belong to a subsystem (Docker,
// Kubernetes, fail2ban) that only some hosts have. There is no MTA here: lhc
// speaks SMTP itself.
var Tools = []check.Tool{
	{Name: "mpstat", RHELPkg: "sysstat", DebPkg: "sysstat", SUSEPkg: "sysstat"},
	{Name: "ss", RHELPkg: "iproute", DebPkg: "iproute2", SUSEPkg: "iproute2"},
	{Name: "fail2ban-client", RHELPkg: "fail2ban", DebPkg: "fail2ban", SUSEPkg: "fail2ban", Optional: true},
	{Name: "rkhunter", RHELPkg: "rkhunter", DebPkg: "rkhunter", SUSEPkg: "rkhunter", Optional: true},
	{Name: "docker", RHELPkg: "docker-ce", DebPkg: "docker.io", SUSEPkg: "docker", Optional: true},
	{Name: "kubectl", RHELPkg: "kubernetes-client", DebPkg: "kubectl", SUSEPkg: "kubernetes-client", Optional: true},
	{Name: "crictl", RHELPkg: "cri-tools", DebPkg: "cri-tools", SUSEPkg: "cri-tools", Optional: true},
}

func (c Check) Run(_ context.Context, env *check.Env) *check.Section {
	s := check.NewSection("tools", c.Meta().Title)
	for _, t := range Tools {
		label := t.Name
		if t.Optional {
			label = "[optional] " + t.Name
		}
		if _, ok := env.Runner.LookPath(t.Name); ok {
			s.Add(label, "Installed", check.OK)
			continue
		}
		if t.Optional {
			// An optional tool the host does not have is not a finding, it is
			// a fact about what this machine is for. Telling a plain web
			// server to install docker — or kubectl — every single day is
			// pure noise, so optional tools appear only when present.
			continue
		}
		// Never CAUTION: a tool that was missing yesterday is missing today
		// for the same reason, so it can only ever be permanent noise.
		// `lhc tools --install` is the way to act on this.
		s.Add(label, "Not installed — run: "+t.InstallCmd(env.Host.PkgManager), check.Info,
			check.Detail("Reduces coverage of some checks"))
		s.NeedTool(t)
	}
	return s
}
