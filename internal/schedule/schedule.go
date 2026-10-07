// Package schedule spreads scheduled runs over a window and installs the
// timer (systemd) or crontab entry that starts them.
//
// Every host in a fleet installs the same time, so at 00:07 every one of them
// would start reading /proc, walking /etc and calling df, dnf and kubectl —
// often on top of the backup window. The check then reports the CPU spike it
// caused itself. So the schedule fires at a fixed, readable time and the RUN
// waits a random slice of the window first; the delay is re-drawn every time,
// and independent uniform draws are all a fleet needs to spread out.
package schedule

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/thyarles/lhc-go/internal/config"
)

// DefaultWindow is used when random_window cannot be read.
const DefaultWindow = 8 * time.Hour

// ParseWindow reads 4h, 90m, 2h30m or a bare number of HOURS.
func ParseWindow(v string) (time.Duration, error) { return config.ParseDuration(v) }

// Window is the configured window, or DefaultWindow — never zero — when the
// value cannot be read. random: true says the operator wants the fleet spread
// out; a typo is a poor reason to send every host back to starting together.
// The warning goes to warn (the run's log).
func Window(raw string, warn io.Writer) time.Duration {
	w, err := ParseWindow(raw)
	if err != nil {
		_, _ = fmt.Fprintf(warn, "  ! schedule.random_window: %v — falling back to %s\n", err, FmtDuration(DefaultWindow))
		return DefaultWindow
	}
	return w
}

// FmtDuration renders minutes like a clock: 0m, 37m, 2h37m, 4h00m.
func FmtDuration(d time.Duration) string {
	mins := int(d / time.Minute)
	if h := mins / 60; h > 0 {
		return fmt.Sprintf("%dh%02dm", h, mins%60)
	}
	return fmt.Sprintf("%dm", mins)
}

// Draw is a uniform delay in [0, window], in whole minutes. Whole minutes
// keep the log line ("checks start at 09:37") honest against the clock; the
// upper bound is inclusive so the tail of the window is reachable.
func Draw(window time.Duration, rng *rand.Rand) time.Duration {
	if window <= 0 {
		return 0
	}
	n := int64(window/time.Minute) + 1
	var m int64
	if rng != nil {
		m = rng.Int64N(n)
	} else {
		m = rand.Int64N(n) //nolint:gosec // jitter, not security
	}
	return time.Duration(m) * time.Minute
}

// Delay announces and serves the random delay of a scheduled run, returning
// the time actually waited. The announcement is written BEFORE the sleep, so
// a log read at 02:30 shows a host that is waiting rather than one that hung.
// The sleep ends early if ctx is cancelled.
func Delay(ctx context.Context, s config.Schedule, log io.Writer, now func() time.Time) time.Duration {
	if !s.Random {
		return 0
	}
	window := Window(s.RandomWindow, log)
	if e := s.Every.D(); e > 0 && window > e {
		// A delay longer than the gap between runs would overlap the next.
		window = e - time.Minute
	}
	d := Draw(window, nil)
	stamp := now().Format("[2006-01-02 15:04:05]")
	if d == 0 {
		_, _ = fmt.Fprintf(log, "%s Random delay: starting now (drew 0m)\n", stamp)
		return 0
	}
	_, _ = fmt.Fprintf(log, "%s Random delay %s of a %s window — checks start at %s\n",
		stamp, FmtDuration(d), FmtDuration(window), now().Add(d).Format("15:04"))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return d
	case <-ctx.Done():
		return 0
	}
}

// Times lists the HH:MM of every run in a day: the configured time, then
// every `every` after it, wrapping round midnight, sorted.
func Times(s config.Schedule) ([][2]int, error) {
	h, m, err := config.ParseTime(s.Time)
	if err != nil {
		return nil, err
	}
	every := s.Every.D()
	if every <= 0 || every >= 24*time.Hour {
		return [][2]int{{h, m}}, nil
	}
	step := int(every / time.Hour)
	var hours []int
	for i := range 24 / step {
		hours = append(hours, (h+i*step)%24)
	}
	// Sorted so the cron and OnCalendar lines read naturally.
	slices.Sort(hours)
	out := make([][2]int, len(hours))
	for i, hh := range hours {
		out[i] = [2]int{hh, m}
	}
	return out, nil
}

// Next is the first scheduled time strictly after now.
func Next(s config.Schedule, now time.Time) (time.Time, error) {
	times, err := Times(s)
	if err != nil {
		return time.Time{}, err
	}
	for day := 0; day < 2; day++ {
		base := time.Date(now.Year(), now.Month(), now.Day()+day, 0, 0, 0, 0, now.Location())
		for _, t := range times {
			c := base.Add(time.Duration(t[0])*time.Hour + time.Duration(t[1])*time.Minute)
			if c.After(now) {
				return c, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("no next run found")
}

// Describe is the schedule in words, for `lhc install`.
func Describe(s config.Schedule) string {
	times, err := Times(s)
	if err != nil {
		return err.Error()
	}
	var at string
	for i, t := range times {
		if i > 0 {
			at += ", "
		}
		at += fmt.Sprintf("%02d:%02d", t[0], t[1])
	}
	what := "daily at " + at
	if len(times) > 1 {
		what = "every " + s.Every.String() + " (" + at + ")"
	}
	if !s.Random {
		return "runs " + what
	}
	w, err := ParseWindow(s.RandomWindow)
	if err != nil {
		w = DefaultWindow
	}
	if e := s.Every.D(); e > 0 && w > e {
		w = e - time.Minute
	}
	return fmt.Sprintf("fires %s, then waits a random 0–%s before running", what, FmtDuration(w))
}
