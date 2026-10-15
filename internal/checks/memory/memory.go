// Package memory reports RAM and swap usage, with the change since the
// previous run.
package memory

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	Caution      float64 `yaml:"caution"`
	Unhealthy    float64 `yaml:"unhealthy"`
}

type Check struct{}

func (Check) Meta() check.Meta { return check.Meta{Name: "memory", Title: "Memory Usage", Order: 30} }

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, Caution: 80, Unhealthy: 95}
}

// Validate keeps the thresholds in order.
func (c *Config) Validate() error {
	if c.Caution > c.Unhealthy {
		return fmt.Errorf("caution (%g) is above unhealthy (%g)", c.Caution, c.Unhealthy)
	}
	return nil
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

const stateKey = "memory_pct"

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("memory", c.Meta().Title)
	var prev map[string]float64
	if !env.State.Load(stateKey, &prev) {
		prev = nil
	}
	current := map[string]float64{}

	// free(1) rather than /proc/meminfo: its idea of "used" is what an admin
	// sees when they log in and check, and the report must agree with it.
	availCol := -1
	for _, line := range strings.Split(env.Runner.Run(ctx, "free", "-b").Stdout, "\n") {
		p := strings.Fields(line)
		// The header names the columns. Before procps 3.3.10 the place of
		// "available" held "cached", so its position alone proves nothing.
		if i := slices.Index(p, "available"); i >= 0 && p[0] == "total" {
			availCol = i + 1 // the data rows lead with "Mem:"
			continue
		}
		if len(p) < 3 {
			continue
		}
		label := strings.TrimSuffix(p[0], ":")
		if label != "Mem" && label != "Swap" {
			continue
		}
		total, err1 := strconv.ParseInt(p[1], 10, 64)
		used, err2 := strconv.ParseInt(p[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		// Kubernetes nodes run without swap on purpose. "0.0% used (0 B of
		// 0 B)" read like an idle swap; disabled is a different fact.
		if label == "Swap" && total == 0 {
			s.Add(label, "Disabled", check.OK)
			continue
		}
		// For RAM, free(1)'s "available" (procps 3.3.10 and up): its "free"
		// leaves out the page cache the kernel hands back on demand, and
		// read "31% · 1014 MB free of 15 GB" on a host with 10 GB to spare.
		// "avail" is df's word for it, and it fits the text report.
		free, word := total-used, "free"
		col := 3
		if label == "Mem" && availCol > 0 {
			col, word = availCol, "avail"
		}
		if len(p) > col {
			f, err := strconv.ParseInt(p[col], 10, 64)
			if err != nil {
				continue
			}
			free = f
		}
		pct := 0.0
		if total > 0 {
			pct = float64(used) / float64(total) * 100
		}
		st := level(pct, cfg.Caution, cfg.Unhealthy)
		// Only RAM interrupts anyone. A full swap on its own is a symptom
		// worth reading about in the report, not a page.
		if st == check.Unhealthy && label == "Mem" {
			s.Alert(check.Unhealthy, fmt.Sprintf("RAM at %.0f%%", pct))
		}
		current[label] = math.Round(pct*10) / 10
		if label == "Mem" {
			s.Fact("ram", fmt.Sprintf("%.0f%%", pct), st)
		}
		s.Add(label,
			usage(pct, free, total, word),
			st, check.Meter(pct), check.Delta(deltaNote(prev, label, pct)))
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving memory usage", "err", err)
	}
	return s
}

// usage is the value beside the meter. It fits the 78-column text report,
// where "15.2% used  (279 MB of 1 GB, 1 GB free)" lost its free figure.
func usage(pct float64, free, total int64, word string) string {
	return fmt.Sprintf("%.0f%% · %s %s of %s", pct, size(free), word, size(total))
}

// deltaNote is the signed change since the previous run. A percentage on its
// own does not say whether you have days or hours. Moves under half a point
// are noise and leave the column empty: silence is the message.
func deltaNote(prev map[string]float64, key string, now float64) string {
	before, ok := prev[key]
	if !ok {
		return ""
	}
	diff := now - before
	if math.Abs(diff) < 0.5 {
		return ""
	}
	return fmt.Sprintf("%+.1f pts since last run", diff)
}

// size prints n bytes in the largest unit that keeps it under 1024, with
// one decimal below 10. Whole units read "1 GB free of 1 GB" on a /boot a
// quarter full. Values that would round to 1024 move up a unit.
func size(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f, i := float64(n), 0
	for i < len(units)-1 && math.Abs(f) >= 1023.5 {
		f /= 1024
		i++
	}
	if i > 0 && math.Abs(f) < 9.95 {
		return fmt.Sprintf("%.1f %s", f, units[i])
	}
	return fmt.Sprintf("%.0f %s", f, units[i])
}
