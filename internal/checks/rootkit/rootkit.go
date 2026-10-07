// Package rootkit looks for rootkit indicators: rkhunter warnings, files
// known rootkits leave behind, and processes hidden from ps.
package rootkit

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "rootkit", Title: "Rootkit Indicators", Order: 190}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

const rkhunterTimeout = 120 * time.Second

// suspicious are paths left behind by well-known rootkits.
var suspicious = []string{
	"/usr/lib/.libc.so", "/usr/lib/.so", "/lib/.so",
	"/usr/bin/.sniffer", "/usr/bin/bsd-port",
	"/usr/bin/sshd1", "/usr/bin/rsyncd",
	"/dev/.hdd", "/dev/.udev",
	"/tmp/.ICE-unix/.X0-lock",
}

var warnRE = regexp.MustCompile(`(?i)warn`)

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("rootkit", c.Meta().Title)
	rkhunter(ctx, s, env)

	var found []string
	for _, p := range suspicious {
		if env.Runner.Exists(p) {
			found = append(found, p)
		}
	}
	for _, p := range found {
		s.Add("Suspicious Path", p, check.Unhealthy)
		s.Alert(check.Unhealthy, "Suspicious file/dir found: "+p)
	}
	if len(found) == 0 {
		s.Add("Known Rootkit Paths", "None found", check.OK)
	}

	hiddenProcesses(ctx, s, env)
	return s
}

func rkhunter(ctx context.Context, s *check.Section, env *check.Env) {
	if _, ok := env.Runner.LookPath("rkhunter"); !ok {
		s.Add("rkhunter", "Not installed (optional)", check.Info)
		s.NeedTool(check.Tool{Name: "rkhunter", Optional: true})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, rkhunterTimeout)
	defer cancel()
	out := strings.Split(env.Runner.Run(ctx, "rkhunter", "--check", "--sk", "--rwo").Stdout, "\n")
	var warnings []string
	for _, l := range out[:min(30, len(out))] {
		if warnRE.MatchString(l) {
			warnings = append(warnings, strings.TrimSpace(l))
		}
	}
	if len(warnings) == 0 {
		s.Add("rkhunter", "No warnings", check.OK)
		return
	}
	for _, w := range warnings[:min(10, len(warnings))] {
		s.Add("rkhunter", trunc(w, 100), check.Caution)
	}
	s.Alert(check.Caution, "rkhunter warning")
}

// hiddenProcesses compares /proc with ps.
//
// Comparing a single /proc listing against a single `ps` run is a race: any
// process that starts or exits between the two looks hidden. On a busy host
// that produced dozens of phantom "hidden processes" every day. So the ps call
// is bracketed by two /proc reads and only PIDs present in BOTH are kept: a
// real hidden process persists, a race artifact does not. Survivors must also
// be missing from a second ps run and still exist in /proc.
func hiddenProcesses(ctx context.Context, s *check.Section, env *check.Env) {
	r := env.Runner
	before := procPIDs(env)
	first := r.Run(ctx, "ps", "-eo", "pid", "--no-headers")
	after := procPIDs(env)

	// Without a ps listing every PID would look hidden.
	if !first.OK() || first.Stdout == "" {
		s.Add("Process Visibility", "ps unavailable — not checked", check.Info)
		return
	}
	seen := pidSet(first.Stdout)
	var hidden []int
	for pid := range before {
		if after[pid] && !seen[pid] && pid > 2 {
			hidden = append(hidden, pid)
		}
	}
	if len(hidden) > 0 {
		again := pidSet(r.Run(ctx, "ps", "-eo", "pid", "--no-headers").Stdout)
		hidden = slices.DeleteFunc(hidden, func(pid int) bool {
			return again[pid] || !r.Exists("/proc/"+strconv.Itoa(pid))
		})
	}
	if len(hidden) == 0 {
		s.Add("Process Visibility", "OK", check.OK)
		return
	}
	slices.Sort(hidden)
	shown := make([]string, 0, 10)
	for _, pid := range hidden[:min(10, len(hidden))] {
		shown = append(shown, strconv.Itoa(pid))
	}
	s.Add("Hidden Processes", fmt.Sprintf("%d PID(s) in /proc not visible in ps: %s", len(hidden), strings.Join(shown, ", ")), check.Caution)
	s.Alert(check.Caution, fmt.Sprintf("%d potentially hidden process(es)", len(hidden)))
}

// procPIDs lists the numeric entries of /proc.
func procPIDs(env *check.Env) map[int]bool {
	out := map[int]bool{}
	entries, err := env.Runner.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			out[pid] = true
		}
	}
	return out
}

func pidSet(out string) map[int]bool {
	set := map[int]bool{}
	for _, f := range strings.Fields(out) {
		if pid, err := strconv.Atoi(f); err == nil {
			set[pid] = true
		}
	}
	return set
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
