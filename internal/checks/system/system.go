// Package system reports what the machine is: name, OS, kernel, uptime, and
// whether it rebooted since the previous run.
package system

import (
	"context"
	"fmt"
	"math"
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
	return check.Meta{Name: "system", Title: "System Information", Order: 10}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

func firstLabel(s string) string {
	f, _, _ := strings.Cut(s, ".")
	return strings.ToLower(f)
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("system", c.Meta().Title)
	r, h := env.Runner, env.Host
	now := env.Now()

	s.Add("Date", now.Format("2006-01-02 15:04"), check.OK)
	s.Add("Hostname", h.Label, check.OK)
	// When the resolved name disagrees with the kernel's, say so once. That
	// mismatch is how three RKE2 nodes came to call themselves by one VIP
	// name, and it is invisible until something prints both.
	if h.Resolved != "" && firstLabel(h.Resolved) != firstLabel(h.Kernel) {
		s.Add("Resolved name", h.Resolved+"  (shared/VIP name — not used as this host's identity)", check.Info)
	}
	osName := h.OS["PRETTY_NAME"]
	if osName == "" {
		osName = "unknown"
	}
	s.Add("OS", osName, check.OK)
	s.Add("Kernel", or(r.Run(ctx, "uname", "-r").Stdout, "unknown"), check.OK)
	s.Add("Architecture", or(r.Run(ctx, "uname", "-m").Stdout, "unknown"), check.OK)

	if b, err := r.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.Add("Uptime", Uptime(time.Duration(secs)*time.Second), check.OK)
			}
		}
	}

	// A reboot explains new PIDs, rewritten /etc files and reset counters
	// all at once, so say it plainly instead of leaving the reader to infer it.
	btime := BootTime(r.ReadFile)
	var prev float64
	rebooted := btime > 0 && env.State.Load("boot_time", &prev) && prev > 0 && math.Abs(prev-btime) > 60
	boot := "unknown"
	if btime > 0 {
		boot = time.Unix(int64(btime), 0).In(now.Location()).Format("2006-01-02 15:04")
	}
	if rebooted {
		s.Add("Last Boot", boot, check.Caution, check.Detail("host rebooted since the previous run"))
		s.Alert(check.Caution, "Host rebooted since the previous run")
	} else {
		s.Add("Last Boot", boot, check.OK)
	}
	if btime > 0 {
		if err := env.State.Save("boot_time", btime); err != nil {
			env.Log.Warn("saving boot time", "err", err)
		}
	}

	s.Add("CPU Cores", or(r.Run(ctx, "nproc").Stdout, "unknown"), check.OK)
	for _, line := range strings.Split(r.Run(ctx, "lscpu").Stdout, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Model name" {
			if v = strings.Join(strings.Fields(v), " "); v != "" {
				s.Add("CPU Model", v, check.OK)
			}
			break
		}
	}
	return s
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// BootTime is the epoch second of the last boot from /proc/stat, or 0.
// Exported because the /etc check also needs it.
func BootTime(read func(string) ([]byte, error)) float64 {
	b, err := read("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err == nil {
				return v
			}
		}
	}
	return 0
}

// Uptime renders like `uptime -p`: "up 2 weeks, 3 days, 4 hours, 5 minutes".
func Uptime(d time.Duration) string {
	mins := int(d / time.Minute)
	parts := []struct {
		n    int
		unit string
	}{
		{mins / (60 * 24 * 7), "week"},
		{mins / (60 * 24) % 7, "day"},
		{mins / 60 % 24, "hour"},
		{mins % 60, "minute"},
	}
	var out []string
	for _, p := range parts {
		if p.n == 0 {
			continue
		}
		u := p.unit
		if p.n != 1 {
			u += "s"
		}
		out = append(out, fmt.Sprintf("%d %s", p.n, u))
	}
	if len(out) == 0 {
		return "up 0 minutes"
	}
	return "up " + strings.Join(out, ", ")
}
