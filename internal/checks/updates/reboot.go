package updates

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/runner"
)

// "Days since reboot" alone does not say whether a reboot matters. What
// matters is an installed update (a kernel, glibc, systemd) that only takes
// effect after one. Every family can tell; each in its own way.

const (
	rebootStateKey = "reboot_since"
	// Its own budget: a slow mirror can use up the update query's.
	rebootTimeout = 60 * time.Second
)

type rebootAnswer struct {
	required bool
	reason   string
	names    []string // the packages that need it, when the tool says (rpm)
	file     string   // a file whose change time dates it (apt)
	kernel   string   // the newest installed kernel, when the comparison decided
}

// reboot adds the "Reboot Required" row. When no method gives an answer
// there is no row at all: unknown is not a finding.
func reboot(ctx context.Context, s *check.Section, env *check.Env, cfg *Config) {
	ctx, cancel := runner.WithTimeout(ctx, rebootTimeout)
	defer cancel()
	a, ok := rebootRequired(ctx, env.Runner, env.Host.PkgManager)
	if !ok {
		return
	}
	now := env.Now()
	if !a.required {
		s.Add("Reboot Required", "No", check.OK)
		// Cleared, so the next pending reboot starts its own clock.
		if err := env.State.Save(rebootStateKey, ""); err != nil {
			env.Log.Warn("saving reboot state", "err", err)
		}
		return
	}

	// The update that made the reboot necessary dates it. The first run
	// that saw it is only the fallback: on a first run it said "since
	// today" about a kernel installed ten weeks earlier.
	var stamp string
	env.State.Load(rebootStateKey, &stamp)
	since := installedSince(ctx, env.Runner, env.Host.PkgManager, a)
	if since.IsZero() || since.After(now) {
		var err error
		since, err = time.Parse(time.RFC3339, stamp)
		if err != nil || since.After(now) {
			since = now
		}
	}
	if v := since.UTC().Format(time.RFC3339); v != stamp {
		if err := env.State.Save(rebootStateKey, v); err != nil {
			env.Log.Warn("saving reboot state", "err", err)
		}
	}

	// INFO while it is recent, then CAUTION, then UNHEALTHY. The alert
	// names the threshold, not the age, so it keeps one identity while it
	// stays open, and with digits removed the two steps read alike: the
	// second is the same condition getting worse.
	age := now.Sub(since)
	over := func(days int) bool { return days > 0 && age > time.Duration(days)*24*time.Hour }
	st := check.Info
	switch {
	case over(cfg.RebootUnhealthyDays):
		st = check.Unhealthy
		s.Alert(st, fmt.Sprintf("Reboot pending for more than %d days", cfg.RebootUnhealthyDays))
	case over(cfg.RebootCautionDays):
		st = check.Caution
		s.Alert(st, fmt.Sprintf("Reboot pending for more than %d days", cfg.RebootCautionDays))
	}
	var opts []check.RowOpt
	if a.reason != "" {
		opts = append(opts, check.Detail(a.reason))
	}
	s.Add("Reboot Required", "Yes — since "+since.In(now.Location()).Format("2006-01-02"), st, opts...)
	s.Fact("reboot_required", "yes", st)
}

// rpmKernelNames are the packages that carry a kernel on the rpm families.
var rpmKernelNames = []string{"kernel", "kernel-core", "kernel-default"}

// installedSince is when the update that requires the reboot was installed:
// the earliest install after boot among the packages that need it (rpm),
// or the change time of the file that says so (apt). Change time, because
// dpkg keeps a file's build date as its modification time. The zero time
// when nothing tells.
func installedSince(ctx context.Context, r runner.Runner, pm string, a rebootAnswer) time.Time {
	boot, ok := bootTime(r)
	if !ok {
		return time.Time{}
	}
	var times []int64
	switch pm {
	case "apt-get":
		file := a.file
		if file == "" && a.kernel != "" {
			file = "/boot/vmlinuz-" + a.kernel
		}
		if file == "" {
			return time.Time{}
		}
		if t, err := strconv.ParseInt(strings.TrimSpace(r.Run(ctx, "stat", "-c", "%Z", file).Stdout), 10, 64); err == nil {
			times = append(times, t)
		}
	case "dnf", "yum", "zypper":
		want := a.names
		if len(want) == 0 {
			want = rpmKernelNames
		}
		for _, l := range strings.Split(r.Run(ctx, "rpm", "-qa", "--qf", "%{INSTALLTIME} %{NAME}\n").Stdout, "\n") {
			f := strings.Fields(l)
			if len(f) != 2 || !slices.Contains(want, f[1]) {
				continue
			}
			if t, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				times = append(times, t)
			}
		}
	}
	var first int64
	for _, t := range times {
		if t > boot && (first == 0 || t < first) {
			first = t
		}
	}
	if first == 0 {
		return time.Time{}
	}
	return time.Unix(first, 0)
}

// bootTime is btime from /proc/stat, in Unix seconds.
func bootTime(r runner.Runner) (int64, bool) {
	b, err := r.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			t, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return t, err == nil && t > 0
		}
	}
	return 0, false
}

// rebootRequired tries the family's own answer first, then compares the
// newest installed kernel with the running one. The first method that
// answers wins.
func rebootRequired(ctx context.Context, r runner.Runner, pm string) (rebootAnswer, bool) {
	switch pm {
	case "apt-get":
		// Written by the kernel and libc hooks of update-notifier (Ubuntu)
		// or unattended-upgrades (Debian). Without them it never appears,
		// so its absence is not a "no": the kernel comparison decides.
		if r.Exists("/var/run/reboot-required") {
			return rebootAnswer{required: true, reason: aptReason(r), file: "/var/run/reboot-required"}, true
		}
		return kernelAnswer(ctx, r, debKernels)
	case "dnf", "yum":
		if a, ok := needsRestarting(ctx, r, pm); ok {
			return a, true
		}
	case "zypper":
		if r.Exists("/run/reboot-needed") {
			return rebootAnswer{required: true}, true
		}
		switch res := r.Run(ctx, "zypper", "--non-interactive", "needs-rebooting"); {
		case res.Err != nil:
		case res.Code == 102:
			return rebootAnswer{required: true}, true
		case res.Code == 0:
			return rebootAnswer{}, true
		}
	default:
		return rebootAnswer{}, false
	}
	return kernelAnswer(ctx, r, rpmKernels)
}

// aptReason lists the packages from reboot-required.pkgs, once each.
func aptReason(r runner.Runner) string {
	b, err := r.ReadFile("/var/run/reboot-required.pkgs")
	if err != nil {
		return ""
	}
	return updated(strings.Fields(string(b)))
}

// updated is "updated since boot: a, b, c", at most five names.
func updated(names []string) string {
	var uniq []string
	for _, n := range names {
		if !slices.Contains(uniq, n) {
			uniq = append(uniq, n)
		}
	}
	if len(uniq) == 0 {
		return ""
	}
	more := ""
	if len(uniq) > 5 {
		more = fmt.Sprintf(" and %d more", len(uniq)-5)
		uniq = uniq[:5]
	}
	return "updated since boot: " + strings.Join(uniq, ", ") + more
}

// needsRestarting runs `needs-restarting -r` (yum-utils / dnf-utils), or the
// dnf plugin of the same name. It exits 1 when a reboot is required, 0 when
// not. A missing dnf plugin also exits 1, so the code is trusted only next
// to the sentence that explains it.
func needsRestarting(ctx context.Context, r runner.Runner, pm string) (rebootAnswer, bool) {
	var res runner.Result
	switch _, ok := r.LookPath("needs-restarting"); {
	case ok:
		res = r.Run(ctx, "needs-restarting", "-r")
	case pm == "dnf":
		res = r.Run(ctx, "dnf", "needs-restarting", "-r")
	default:
		return rebootAnswer{}, false
	}
	if res.Err != nil || !strings.Contains(strings.ToLower(res.Stdout), "reboot") {
		return rebootAnswer{}, false
	}
	switch res.Code {
	case 0:
		return rebootAnswer{}, true
	case 1:
		// "  * kernel-core" (dnf) or "  kernel -> 3.10.0-1160.119.1.el7" (yum).
		var names []string
		for _, l := range strings.Split(res.Stdout, "\n") {
			l = strings.TrimSpace(l)
			if n, ok := strings.CutPrefix(l, "* "); ok {
				names = append(names, strings.TrimSpace(n))
			} else if n, _, ok := strings.Cut(l, " -> "); ok {
				names = append(names, strings.TrimSpace(n))
			}
		}
		return rebootAnswer{required: true, reason: updated(names), names: names}, true
	}
	return rebootAnswer{}, false
}

// kernelAnswer compares the running kernel with the newest installed one.
// It answers only when the running kernel is itself among the installed
// ones: a custom or foreign kernel (a container, WSL) proves nothing.
func kernelAnswer(ctx context.Context, r runner.Runner, installed func(context.Context, runner.Runner, string) []string) (rebootAnswer, bool) {
	running := r.Run(ctx, "uname", "-r").Stdout
	if running == "" {
		return rebootAnswer{}, false
	}
	kernels := installed(ctx, r, running)
	if len(kernels) == 0 || !slices.ContainsFunc(kernels, func(k string) bool { return sameKernel(k, running) }) {
		return rebootAnswer{}, false
	}
	if newest := kernels[0]; !sameKernel(newest, running) {
		return rebootAnswer{required: true, reason: fmt.Sprintf("kernel %s installed, %s running", newest, running), kernel: newest}, true
	}
	return rebootAnswer{}, true
}

// rpmKernels lists the installed kernels, newest install first, as
// version-release.arch ("5.14.0-427.13.1.el9_4.x86_64"). rpm exits 1 when
// one of the names is not installed, which is normal: kernel-core does not
// exist on RHEL 7, kernel-default exists only on SUSE.
func rpmKernels(ctx context.Context, r runner.Runner, _ string) []string {
	var out []string
	for _, l := range strings.Split(r.Run(ctx, "rpm", "-q", "--last", "kernel", "kernel-core", "kernel-default").Stdout, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || f[0] == "package" {
			continue
		}
		for _, p := range []string{"kernel-default-", "kernel-core-", "kernel-"} {
			if v, ok := strings.CutPrefix(f[0], p); ok {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

// debKernels lists the installed linux-image packages of the running
// flavour as kernel release names ("6.1.0-26-amd64"), newest first.
func debKernels(ctx context.Context, r runner.Runner, running string) []string {
	flavour := debFlavour(running)
	if flavour == "" {
		return nil
	}
	var out []string
	res := r.Run(ctx, "dpkg-query", "-W", "-f=${db:Status-Abbrev} ${Package}\n", "linux-image-*")
	for _, l := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || f[0] != "ii" {
			continue
		}
		v, ok := strings.CutPrefix(f[1], "linux-image-")
		if !ok {
			continue
		}
		v = strings.TrimSuffix(strings.TrimPrefix(v, "unsigned-"), "-unsigned")
		if debFlavour(v) == flavour && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return versionCmp(b, a) })
	return out
}

// debFlavour is what follows "6.1.0-26-" in a Debian or Ubuntu kernel
// release ("amd64", "cloud-amd64", "generic"); "" for anything else, which
// covers the meta packages (linux-image-amd64) and foreign kernels.
func debFlavour(release string) string {
	ver, rest, ok := strings.Cut(release, "-")
	if !ok || strings.Count(ver, ".") < 1 || !digitsAndDots(ver) {
		return ""
	}
	abi, flavour, ok := strings.Cut(rest, "-")
	if !ok || abi == "" || strings.Trim(abi, "0123456789") != "" || flavour == "" || strings.Contains(flavour, "-dbg") {
		return ""
	}
	return flavour
}

func digitsAndDots(s string) bool { return s != "" && strings.Trim(s, "0123456789.") == "" }

// versionCmp compares the runs of digits numerically and everything else
// as text: "6.1.0-26" sorts after "6.1.0-9".
func versionCmp(a, b string) int {
	for a != "" && b != "" {
		na, ra := lead(a)
		nb, rb := lead(b)
		if na != nb {
			x, errA := strconv.Atoi(na)
			y, errB := strconv.Atoi(nb)
			if errA == nil && errB == nil && x != y {
				if x < y {
					return -1
				}
				return 1
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
		}
		a, b = ra, rb
	}
	return strings.Compare(a, b)
}

// lead splits off the leading run of digits, or of non-digits.
func lead(s string) (string, string) {
	digit := s[0] >= '0' && s[0] <= '9'
	i := 1
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') == digit {
		i++
	}
	return s[:i], s[i:]
}

// sameKernel compares an installed kernel with `uname -r`. RHEL prints the
// same string; SUSE drops the last release field and appends the flavour
// (package 5.14.21-150500.55.65.1.x86_64, running 5.14.21-150500.55.65-default).
func sameKernel(pkg, running string) bool {
	if pkg == running {
		return true
	}
	p, u := stripArch(pkg), stripArch(strings.TrimSuffix(running, "-default"))
	return p == u || strings.HasPrefix(p, u+".")
}

func stripArch(v string) string {
	for _, a := range []string{".x86_64", ".aarch64", ".ppc64le", ".s390x", ".i686", ".noarch"} {
		if s, ok := strings.CutSuffix(v, a); ok {
			return s
		}
	}
	return v
}
