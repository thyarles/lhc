// Package etc lists files under /etc modified in the last 24 hours and
// flags the ones whose modification matters for security.
package etc

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checks/system"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	// Extra find -path patterns to skip, on top of the built-in list.
	Ignore []string `yaml:"ignore"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "etc", Title: "/etc Modifications (last 24h)", Order: 170}
}

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, Ignore: []string{"/etc/CommVaultRegistryBackups/*"}}
}

// sensitive are the files under /etc whose modification actually means
// something for security. Everything else in /etc changes constantly
// through normal package activity. A trailing "/" or "." is a prefix.
var sensitive = []string{
	"/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow",
	"/etc/sudoers", "/etc/sudoers.d/", "/etc/ssh/sshd_config",
	"/etc/ssh/ssh_config", "/etc/pam.d/", "/etc/crontab", "/etc/cron.",
	"/etc/fstab", "/etc/hosts.allow", "/etc/hosts.deny",
	"/etc/sysctl.conf", "/etc/selinux/config", "/etc/security/",
}

// builtinIgnore is always skipped; Config.Ignore adds to it.
var builtinIgnore = []string{
	"*/mtab", "*/adjtime", "*/ld.so.cache", "*/resolv.conf",
	"*/machine-id", "*/.pwd.lock", "*/blkid.tab*",
	"*/network/interfaces.d/*",
}

const (
	listCap = 40
	timeout = 30 * time.Second
)

// findArgs builds the find invocation. It is an argument list, not a shell
// string, so a pattern with a space or a semicolon stays one argument.
//
// Backup agents write timestamped files into /etc on a schedule (CommVault
// drops a new .zst into /etc/CommVaultRegistryBackups every 90 minutes) and
// each one showed up as a fresh modification. Those are configuration
// backups, not configuration changes, hence the ignore list.
func findArgs(cfg *Config) []string {
	args := []string{"/etc", "-maxdepth", "3", "-type", "f", "-mmin", "-1440"}
	for _, n := range []string{"*.swp", "*.tmp", "*.dpkg-*", "*.rpmnew", "*.rpmsave"} {
		args = append(args, "-not", "-name", n)
	}
	for _, p := range append(slices.Clone(builtinIgnore), cfg.Ignore...) {
		if p = strings.TrimSpace(p); p != "" {
			args = append(args, "-not", "-path", p)
		}
	}
	return args
}

type mtime struct {
	epoch   float64
	display string // "2006-01-02 15:04:05"
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("etc", c.Meta().Title)
	r := env.Runner

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var files []string
	for _, f := range strings.Split(r.Run(ctx, "find", findArgs(cfg)...).Stdout, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		s.Add("Changes", "No files modified in the last 24 hours", check.OK)
		return s
	}
	slices.Sort(files)
	if len(files) > listCap {
		files = files[:listCap]
	}

	// One stat for every file: "%Y %y %n" is epoch, then the human date
	// (date, time, zone), then the name, which may itself contain spaces.
	times := map[string]mtime{}
	out := r.Run(ctx, "stat", append([]string{"-c", "%Y %y %n"}, files...)...).Stdout
	for _, line := range strings.Split(out, "\n") {
		p := strings.SplitN(line, " ", 5)
		if len(p) < 5 {
			continue
		}
		epoch, err := strconv.ParseFloat(p[0], 64)
		if err != nil {
			continue
		}
		disp := p[1] + " " + p[2]
		if len(disp) > 19 {
			disp = disp[:19]
		}
		times[p[4]] = mtime{epoch, disp}
	}

	// Files rewritten by the init/cloud-init/WSL machinery within a minute of
	// boot (hostname, hosts, timezone…) are a side effect of rebooting, not
	// an edit someone made. Treating them as CAUTION made every reboot look
	// like a security event.
	btime := system.BootTime(r.ReadFile)
	var hits []string
	for _, f := range files {
		m, ok := times[f]
		atBoot := ok && btime > 0 && math.Abs(m.epoch-btime) <= 60
		switch {
		case isSensitive(f):
			hits = append(hits, f)
			s.Add(f, m.display, check.Caution, check.Detail("security-relevant file"))
		case atBoot:
			s.Add(f, m.display, check.Info, check.Detail("rewritten at boot"))
		default:
			s.Add(f, m.display, check.Info)
		}
	}

	if len(hits) > 0 {
		// One alert for the whole set, not one per file, which used to bury
		// the genuinely interesting lines under a wall of routine ones.
		msg := "Security-relevant /etc change: " + strings.Join(hits[:min(3, len(hits))], ", ")
		if len(hits) > 3 {
			msg += fmt.Sprintf(" (+%d more)", len(hits)-3)
		}
		s.Alert(check.Caution, msg)
	} else {
		s.Add("Security-Relevant Changes", "None", check.OK)
	}
	return s
}

func isSensitive(f string) bool {
	return slices.ContainsFunc(sensitive, func(p string) bool { return strings.HasPrefix(f, p) })
}
