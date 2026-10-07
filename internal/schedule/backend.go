package schedule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/config"
	"github.com/thyarles/lhc-go/internal/runner"
)

// Backend is what starts scheduled runs.
type Backend string

const (
	Systemd Backend = "systemd"
	Cron    Backend = "cron"
	None    Backend = ""
)

// CronTag marks the crontab line lhc owns, so reinstalling replaces it and
// uninstalling finds it.
const CronTag = "# lhc-managed"

// LegacyCronTag is the marker of the Python version's entry.
const LegacyCronTag = "linux-healthcheck-managed"

// Unit file locations.
var (
	UnitDir     = "/etc/systemd/system"
	ServiceName = "lhc.service"
	TimerName   = "lhc.timer"
)

// Detect picks the backend. systemd needs root (these are system units), a
// running systemd, and version 229 or later: RandomizedDelaySec and
// Persistent arrived in 229, and RHEL 7 ships 219, so RHEL 7 gets cron.
func Detect(ctx context.Context, r runner.Runner, root bool) Backend {
	if root && r.Exists("/run/systemd/system") {
		if v := systemdVersion(r.Run(ctx, "systemctl", "--version").Stdout); v >= 229 {
			return Systemd
		}
	}
	if _, ok := r.LookPath("crontab"); ok {
		return Cron
	}
	return None
}

// systemdVersion reads "systemd 252 (252.22-1~deb12u1)".
func systemdVersion(out string) int {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "systemd" {
		return 0
	}
	v, _ := strconv.Atoi(f[1])
	return v
}

// Job is what the backend runs.
type Job struct {
	Binary string // absolute path to lhc
	Config string // absolute path to the config file
	Log    string // log file, for the crontab redirect
}

// ServiceUnit is the oneshot unit the timer starts. The timer does the random
// delay itself, so the run is told not to add another.
func ServiceUnit(j Job) string {
	return fmt.Sprintf(`# Managed by lhc install. Changes are overwritten on the next install.
[Unit]
Description=Linux Health Check (lhc)
Documentation=https://github.com/thyarles/lhc-go
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s run --scheduled --no-random --config %s
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
`, j.Binary, j.Config)
}

// TimerUnit fires at the schedule's times with systemd's own jitter.
// Persistent=true runs a missed slot at boot, so a host that was off at
// 00:07 still reports that day.
func TimerUnit(s config.Schedule) (string, error) {
	cal, err := OnCalendar(s)
	if err != nil {
		return "", err
	}
	delay := ""
	if s.Random {
		w := Window(s.RandomWindow, os.Stderr)
		if e := s.Every.D(); e > 0 && w > e {
			w = e - time.Minute
		}
		if w > 0 {
			delay = fmt.Sprintf("RandomizedDelaySec=%d\n", int(w.Seconds()))
		}
	}
	return fmt.Sprintf(`# Managed by lhc install. Changes are overwritten on the next install.
[Unit]
Description=Run lhc on its schedule

[Timer]
OnCalendar=%s
%sAccuracySec=1min
Persistent=true

[Install]
WantedBy=timers.target
`, cal, delay), nil
}

// OnCalendar renders the schedule as a systemd calendar spec:
// "*-*-* 00,06,12,18:07:00".
func OnCalendar(s config.Schedule) (string, error) {
	hours, minute, err := hoursAndMinute(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("*-*-* %s:%02d:00", hours, minute), nil
}

// CronLine renders the managed crontab entry. The schedule's own random delay
// is applied by the run (--scheduled reads it from the config at run time,
// so changing it needs no reinstall).
func CronLine(s config.Schedule, j Job) (string, error) {
	times, err := Times(s)
	if err != nil {
		return "", err
	}
	hs := make([]string, len(times))
	for i, t := range times {
		hs[i] = strconv.Itoa(t[0])
	}
	return fmt.Sprintf("%d %s * * * %s run --scheduled --config %s >>%s 2>&1  %s",
		times[0][1], strings.Join(hs, ","), j.Binary, j.Config, j.Log, CronTag), nil
}

func hoursAndMinute(s config.Schedule) (string, int, error) {
	times, err := Times(s)
	if err != nil {
		return "", 0, err
	}
	var hs []string
	for _, t := range times {
		hs = append(hs, fmt.Sprintf("%02d", t[0]))
	}
	return strings.Join(hs, ","), times[0][1], nil
}

// MergeCrontab replaces lhc's line in an existing crontab (or appends it),
// leaving every other line alone.
func MergeCrontab(current, line string) string {
	var kept []string
	for _, l := range strings.Split(current, "\n") {
		if l == "" || strings.Contains(l, CronTag) {
			continue
		}
		kept = append(kept, l)
	}
	if line != "" {
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// HasLegacyCron reports whether the Python version's entry is still there.
func HasLegacyCron(current string) bool { return strings.Contains(current, LegacyCronTag) }

// Installer applies a schedule to the host.
type Installer struct {
	Runner runner.Runner
	Out    func(format string, a ...any)
}

// Install writes and enables the backend's schedule. It is idempotent.
func (in *Installer) Install(ctx context.Context, b Backend, s config.Schedule, j Job) error {
	switch b {
	case Systemd:
		// Never leave both running: a previous cron install would double up.
		if err := in.removeCron(ctx); err != nil {
			return err
		}
		timer, err := TimerUnit(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(UnitDir, ServiceName), []byte(ServiceUnit(j)), 0o644); err != nil { //nolint:gosec // unit files are world-readable by convention
			return err
		}
		if err := os.WriteFile(filepath.Join(UnitDir, TimerName), []byte(timer), 0o644); err != nil { //nolint:gosec // as above
			return err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", TimerName}, {"restart", TimerName}} {
			if res := in.Runner.Run(ctx, "systemctl", args...); !res.OK() {
				return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), firstLine(res.Stderr, res.Err))
			}
		}
		in.Out("  ✓ systemd timer %s enabled", TimerName)
		return nil
	case Cron:
		if err := in.removeSystemd(ctx); err != nil {
			return err
		}
		line, err := CronLine(s, j)
		if err != nil {
			return err
		}
		current := in.Runner.Run(ctx, "crontab", "-l").Stdout
		if err := in.writeCrontab(ctx, MergeCrontab(current, line)); err != nil {
			return err
		}
		in.Out("  ✓ crontab entry: %s", line)
		if HasLegacyCron(current) {
			in.Out("  ! The Python linux-health-check cron entry (%s) is still installed.", LegacyCronTag)
			in.Out("    Remove it with `crontab -e` once lhc has run for a while; lhc does not touch it.")
		}
		return nil
	}
	return errors.New("no scheduler available (no systemd ≥ 229 as root, no crontab); run `lhc serve` under your own supervisor instead")
}

// Uninstall removes whatever lhc installed, from either backend.
func (in *Installer) Uninstall(ctx context.Context) error {
	return errors.Join(in.removeSystemd(ctx), in.removeCron(ctx))
}

func (in *Installer) removeSystemd(ctx context.Context) error {
	timer := filepath.Join(UnitDir, TimerName)
	if _, err := os.Stat(timer); err != nil {
		return nil
	}
	in.Runner.Run(ctx, "systemctl", "disable", "--now", TimerName)
	var errs []error
	for _, f := range []string{timer, filepath.Join(UnitDir, ServiceName)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	in.Runner.Run(ctx, "systemctl", "daemon-reload")
	in.Out("  ✓ systemd timer removed")
	return errors.Join(errs...)
}

func (in *Installer) removeCron(ctx context.Context) error {
	if _, ok := in.Runner.LookPath("crontab"); !ok {
		return nil
	}
	current := in.Runner.Run(ctx, "crontab", "-l").Stdout
	if !strings.Contains(current, CronTag) {
		return nil
	}
	if err := in.writeCrontab(ctx, MergeCrontab(current, "")); err != nil {
		return err
	}
	in.Out("  ✓ crontab entry removed")
	return nil
}

// writeCrontab installs a whole crontab from a file: the runner has no stdin,
// and `crontab FILE` is POSIX.
func (in *Installer) writeCrontab(ctx context.Context, content string) error {
	if content == "" {
		if res := in.Runner.Run(ctx, "crontab", "-r"); !res.OK() && !strings.Contains(res.Stderr, "no crontab") {
			return fmt.Errorf("crontab -r: %s", firstLine(res.Stderr, res.Err))
		}
		return nil
	}
	f, err := os.CreateTemp("", "lhc-crontab-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if res := in.Runner.Run(ctx, "crontab", f.Name()); !res.OK() {
		return fmt.Errorf("crontab: %s", firstLine(res.Stderr, res.Err))
	}
	return nil
}

func firstLine(s string, err error) string {
	if s == "" && err != nil {
		return err.Error()
	}
	l, _, _ := strings.Cut(s, "\n")
	return l
}
