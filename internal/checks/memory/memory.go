// Package memory reports RAM and swap usage, with the change since the
// previous run.
package memory

import (
	"context"
	"fmt"
	"math"
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
	for _, line := range strings.Split(env.Runner.Run(ctx, "free", "-b").Stdout, "\n") {
		p := strings.Fields(line)
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
		free := total - used
		if len(p) > 3 {
			f, err := strconv.ParseInt(p[3], 10, 64)
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
			fmt.Sprintf("%.1f%% used  (%s of %s, %s free)", pct, fmtBytes(used), fmtBytes(total), fmtBytes(free)),
			st, check.Meter(pct), check.Delta(deltaNote(prev, label, pct)))
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving memory usage", "err", err)
	}
	return s
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

// fmtBytes matches the Python report: integer-divide by 1024 until the value
// fits, then print it with no decimals ("1 GB", not "1.5 GB").
func fmtBytes(n int64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if n > -1024 && n < 1024 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		n = floorDiv(n, 1024)
	}
	return fmt.Sprintf("%d PB", n)
}

// floorDiv is Python's //, which rounds towards minus infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
