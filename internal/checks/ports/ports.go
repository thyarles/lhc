// Package ports lists the listening TCP and UDP sockets and reports the ones
// that appeared or disappeared since the previous run.
package ports

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/thyarles/lhc-go/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	// Loopback sockets cannot be reached from off the host, and applications
	// churn through random high ports on 127.0.0.1, which is pure noise in a
	// daily report. true lists them anyway.
	ListLocal bool `yaml:"list_local"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "ports", Title: "Listening Ports", Order: 130}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

const stateKey = "ports"

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("ports", c.Meta().Title)
	r := env.Runner

	var raw []string
	switch {
	case has(r, "ss"):
		raw = append(parseSS("tcp", r.Run(ctx, "ss", "-tlnp").Stdout), parseSS("udp", r.Run(ctx, "ss", "-ulnp").Stdout)...)
	case has(r, "netstat"):
		raw = append(parseNetstat("tcp", r.Run(ctx, "netstat", "-tlnp").Stdout), parseNetstat("udp", r.Run(ctx, "netstat", "-ulnp").Stdout)...)
		s.NeedTool(ssTool(true))
	default:
		s.Add("ss / netstat", "Neither available", check.Info)
		s.NeedTool(ssTool(false))
		return s
	}

	// The raw line carries pid= and fd=, which change every time a service
	// restarts. Comparing those made a routine restart look like a new port.
	exposedSet, localSet := map[string]bool{}, map[string]bool{}
	for _, line := range raw {
		e := portKey(line)
		f := strings.Fields(e)
		if len(f) < 2 {
			continue
		}
		if isLoopback(f[1]) {
			localSet[e] = true
		} else {
			exposedSet[e] = true
		}
	}
	exposed, local := sortedKeys(exposedSet), sortedKeys(localSet)
	current := exposed
	if cfg.ListLocal {
		current = sortedKeys(exposedSet, localSet)
	}

	var prev []string
	hasPrev := env.State.Load(stateKey, &prev)
	// State written before this check covered UDP has no protocol prefix.
	// Re-baseline instead of reporting every socket on the host as new.
	if hasPrev && len(prev) > 0 && !slices.ContainsFunc(prev, func(p string) bool {
		return strings.HasPrefix(p, "tcp ") || strings.HasPrefix(p, "udp ")
	}) {
		s.Add("Baseline", "Port list format changed — baseline re-recorded", check.Info)
		hasPrev = false
	}

	if hasPrev {
		added, gone := diff(prev, current)
		if len(added) > 0 {
			s.Separator("New Ports Since Last Run")
			for _, p := range added {
				s.Add("NEW", p, check.Caution)
				s.Alert(check.Caution, "New listening port: "+strings.Join(strings.Fields(p)[:2], " "))
			}
		}
		if len(gone) > 0 {
			s.Separator("Ports No Longer Listening")
			for _, p := range gone {
				s.Add("GONE", p, check.Info)
			}
		}
		if len(added) == 0 && len(gone) == 0 {
			s.Add("Port Changes", "None since last run", check.OK)
		}
	} else {
		s.Add("Baseline", fmt.Sprintf("%d socket(s) recorded (first run)", len(current)), check.Info)
	}

	s.Add("Reachable Sockets", fmt.Sprintf("%d not bound to loopback", len(exposed)), check.Info)
	if !cfg.ListLocal {
		s.Add("Loopback-only Sockets", fmt.Sprintf("%d hidden", len(local)), check.Info,
			check.Detail("set list_local: true under checks.ports to list them"))
	}

	// Listed in full, deliberately uncapped. This is the security-relevant
	// inventory of what the host exposes; truncating it hides exactly what a
	// reader opened the section to check. Loopback filtering keeps it short.
	s.Separator("Listening Sockets")
	for _, line := range current {
		s.Add("", line, check.Info)
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving port baseline", "err", err)
	}
	return s
}

func has(r interface{ LookPath(string) (string, bool) }, name string) bool {
	_, ok := r.LookPath(name)
	return ok
}

func ssTool(optional bool) check.Tool {
	return check.Tool{Name: "ss", RHELPkg: "iproute", DebPkg: "iproute2", SUSEPkg: "iproute2", Optional: optional}
}

// parseSS turns `ss -tlnp` output into "proto local-address process" lines.
// Columns: State Recv-Q Send-Q Local Peer Process; the process column is
// absent without root and may contain spaces (a quoted comm like "Web
// Content"), so everything from the sixth field on is kept.
func parseSS(proto, out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] == "State" || f[0] == "Netid" {
			continue
		}
		proc := ""
		if len(f) > 5 {
			proc = strings.Join(f[5:], " ")
		}
		lines = append(lines, strings.TrimSpace(proto+" "+f[3]+" "+proc))
	}
	return lines
}

var netstatPID = regexp.MustCompile(`^\d+/`)

// parseNetstat does the same for `netstat -tlnp`. UDP rows have no State
// column, so the program is taken from the last field rather than a fixed
// position, and its "1234/" PID prefix is dropped for the same reason pid=
// is dropped from ss output.
func parseNetstat(proto, out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[0], proto) {
			continue
		}
		proc := ""
		if len(f) > 5 && f[len(f)-1] != "LISTEN" {
			proc = netstatPID.ReplaceAllString(f[len(f)-1], "")
		}
		lines = append(lines, strings.TrimSpace(proto+" "+f[3]+" "+proc))
	}
	return lines
}

// splitHostPort splits "host:port", handling IPv6 brackets and zone ids.
// Real inputs: "127.0.0.1:6444", "[::1]:323", "127.0.0.53%lo:53", "*:8472".
func splitHostPort(addr string) (host, port string) {
	if rest, ok := strings.CutPrefix(addr, "["); ok {
		host, rest, _ = strings.Cut(rest, "]")
		return host, strings.TrimLeft(rest, ":")
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", addr
	}
	return addr[:i], addr[i+1:]
}

// isLoopback reports whether nothing outside this host can reach the socket.
// A wildcard bind ("*", "0.0.0.0", "::") is emphatically NOT loopback — that
// is the case a reader most needs to see.
func isLoopback(addr string) bool {
	host, _ := splitHostPort(addr)
	host, _, _ = strings.Cut(host, "%") // drop the "%lo" zone id
	host = strings.TrimSpace(host)
	return strings.HasPrefix(host, "127.") || host == "::1" || host == "localhost"
}

var pidRE = regexp.MustCompile(`,?\s*(pid|fd)=\d+`)

// portKey normalises a socket line to address + process, dropping pid/fd.
func portKey(line string) string {
	line = pidRE.ReplaceAllString(line, "")
	return strings.TrimRight(strings.Join(strings.Fields(line), " "), ",")
}

func sortedKeys(sets ...map[string]bool) []string {
	var out []string
	for _, m := range sets {
		for k := range m {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// diff returns what is in cur but not prev, and what is in prev but not cur,
// each in the order of its source list.
func diff(prev, cur []string) (added, gone []string) {
	in := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, v := range list {
			m[v] = true
		}
		return m
	}
	p, c := in(prev), in(cur)
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
