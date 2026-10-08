// Package cpu reports load average and CPU utilisation.
package cpu

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle      `yaml:",inline"`
	Caution           float64 `yaml:"caution"`
	Unhealthy         float64 `yaml:"unhealthy"`
	LoadCautionMult   float64 `yaml:"load_caution_mult"`
	LoadUnhealthyMult float64 `yaml:"load_unhealthy_mult"`
}

type Check struct{}

func (Check) Meta() check.Meta { return check.Meta{Name: "cpu", Title: "CPU Load", Order: 20} }

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, Caution: 80, Unhealthy: 95, LoadCautionMult: 1.0, LoadUnhealthyMult: 2.0}
}

func level(v, caution, unhealthy float64) check.Status {
	switch {
	case v >= unhealthy:
		return check.Unhealthy
	case v >= caution:
		return check.Caution
	}
	return check.OK
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("cpu", c.Meta().Title)
	r := env.Runner

	ncpus := 1
	if out := r.Run(ctx, "nproc").Stdout; out != "" {
		if n, err := strconv.Atoi(out); err == nil && n > 0 {
			ncpus = n
		}
	}

	if b, err := r.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		var l [3]float64
		ok := len(f) >= 3
		for i := 0; ok && i < 3; i++ {
			v, err := strconv.ParseFloat(f[i], 64)
			l[i], ok = v, err == nil
		}
		if ok {
			tc, tu := cfg.LoadCautionMult*float64(ncpus), cfg.LoadUnhealthyMult*float64(ncpus)
			st := level(l[0], tc, tu)
			if st == check.Unhealthy {
				s.Alert(check.Unhealthy, fmt.Sprintf("Load avg %.2f exceeds %.1f (%d CPUs × %g)", l[0], tu, ncpus, cfg.LoadUnhealthyMult))
			}
			s.Add("Load Average (1/5/15 min)", fmt.Sprintf("%.2f / %.2f / %.2f", l[0], l[1], l[2]), st,
				check.Detail(fmt.Sprintf("Thresholds: caution ≥ %.1f, unhealthy ≥ %.1f", tc, tu)))
			cores := "cores"
			if ncpus == 1 {
				cores = "core"
			}
			s.Fact("load", fmt.Sprintf("%.2f / %d %s", l[0], ncpus, cores), st)
		} else {
			s.Add("Load Average", strings.TrimSpace(string(b)), check.Info)
		}
	}

	if _, ok := r.LookPath("mpstat"); ok {
		perCore(ctx, s, env, cfg)
	} else {
		aggregate(s, env, cfg)
		s.NeedTool(check.Tool{Name: "mpstat", RHELPkg: "sysstat", DebPkg: "sysstat"})
	}
	return s
}

type usage struct {
	id   string
	used float64
	st   check.Status
}

// parseMpstat reads `mpstat -P ALL 1 1`. mpstat prints a live snapshot and
// then "Average:" lines; keeping the last row per CPU id keeps the average.
// The CPU id is the first of the leading tokens that is "all" or a short
// number, which survives both the 12- and 24-hour time formats.
func parseMpstat(out string, cfg *Config) map[string]usage {
	rows := map[string]usage{}
	for _, line := range strings.Split(out, "\n") {
		p := strings.Fields(line)
		if len(p) < 4 {
			continue
		}
		id := ""
		for _, tok := range p[:4] {
			if tok == "all" || (len(tok) <= 3 && isDigits(tok)) {
				id = tok
				break
			}
		}
		if id == "" {
			continue
		}
		idle, err := strconv.ParseFloat(strings.ReplaceAll(p[len(p)-1], ",", "."), 64)
		if err != nil || idle < 0 || idle > 100 {
			continue
		}
		used := 100 - idle
		rows[id] = usage{id, used, level(used, cfg.Caution, cfg.Unhealthy)}
	}
	return rows
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

const listCap = 10

func perCore(ctx context.Context, s *check.Section, env *check.Env, cfg *Config) {
	rows := parseMpstat(env.Runner.Run(ctx, "mpstat", "-P", "ALL", "1", "1").Stdout, cfg)
	if all, ok := rows["all"]; ok {
		if all.st == check.Unhealthy {
			s.Alert(check.Unhealthy, fmt.Sprintf("CPU at %.0f%%", all.used))
		}
		s.Add("CPU (all cores)", fmt.Sprintf("%.1f%% used", all.used), all.st, check.Meter(all.used))
	}
	delete(rows, "all")
	if len(rows) == 0 {
		return
	}
	// On a 96-core box the per-core listing was 97 rows, a third of the
	// whole report, and 96 of them said "0.0% used". Summarise the cores and
	// name only the ones worth looking at.
	var vals []float64
	var busy []usage
	for _, u := range rows {
		vals = append(vals, u.used)
		if u.st.Flagged() {
			busy = append(busy, u)
		}
	}
	slices.SortFunc(vals, func(a, b float64) int { return cmp.Compare(b, a) })
	detail := "no core above the caution threshold"
	if len(busy) > 0 {
		detail = fmt.Sprintf("%d core(s) above the caution threshold", len(busy))
	}
	s.Add("Cores", fmt.Sprintf("%d total · busiest %.1f%% · median %.1f%%", len(rows), vals[0], vals[len(vals)/2]),
		check.OK, check.Detail(detail))
	slices.SortStableFunc(busy, func(a, b usage) int {
		if c := cmp.Compare(b.used, a.used); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	for i, u := range busy {
		if i == listCap {
			s.Add("…", fmt.Sprintf("and %d more core(s) above threshold", len(busy)-listCap), check.Info)
			break
		}
		if u.st == check.Unhealthy {
			s.Alert(check.Unhealthy, fmt.Sprintf("CPU %s at %.0f%%", u.id, u.used))
		}
		s.Add("CPU "+u.id, fmt.Sprintf("%.1f%% used", u.used), u.st, check.Meter(u.used))
	}
}

// aggregate is the fallback without sysstat: utilisation since boot from
// /proc/stat. Coarse, which is why the row says "approx." and asks for
// sysstat.
func aggregate(s *check.Section, env *check.Env, cfg *Config) {
	b, err := env.Runner.ReadFile("/proc/stat")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var vals []float64
		for _, x := range f[1:] {
			if v, err := strconv.ParseFloat(x, 64); err == nil {
				vals = append(vals, v)
			}
		}
		var total float64
		for _, v := range vals {
			total += v
		}
		used := 0.0
		if total > 0 {
			used = (1 - vals[3]/total) * 100
		}
		s.Add("CPU (aggregate, approx.)", fmt.Sprintf("%.1f%% used", used), level(used, cfg.Caution, cfg.Unhealthy),
			check.Detail("Install sysstat for per-core stats"))
		return
	}
}

// Validate keeps the thresholds in order.
func (c *Config) Validate() error {
	if c.Caution > c.Unhealthy {
		return fmt.Errorf("caution (%g) is above unhealthy (%g)", c.Caution, c.Unhealthy)
	}
	if c.LoadCautionMult > c.LoadUnhealthyMult {
		return fmt.Errorf("load_caution_mult (%g) is above load_unhealthy_mult (%g)", c.LoadCautionMult, c.LoadUnhealthyMult)
	}
	return nil
}
