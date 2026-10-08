package updates

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

// Real output, from RHEL 9 (dnf-utils), RHEL 7 (yum-utils), rpm and dpkg.
const (
	dnfRebootNeeded = `Core libraries or services have been updated since boot-up:
  * glibc
  * kernel
  * systemd

Reboot is required to fully utilize these updates.
More information: https://access.redhat.com/solutions/27943`

	dnfNoReboot = `No core libraries or services have been updated since boot-up.
Reboot should not be necessary.`

	yumRebootNeeded = `Core libraries or services have been updated:
  kernel -> 3.10.0-1160.119.1.el7

Reboot is required to ensure that your system benefits from these updates.

More information:
https://access.redhat.com/solutions/27943`

	rpmKernelsEL9 = `kernel-core-5.14.0-427.13.1.el9_4.x86_64      Tue 30 Apr 2024 10:12:01 AM UTC
kernel-5.14.0-427.13.1.el9_4.x86_64           Tue 30 Apr 2024 10:12:01 AM UTC
kernel-core-5.14.0-362.8.1.el9_3.x86_64       Mon 13 Nov 2023 09:01:22 AM UTC
kernel-5.14.0-362.8.1.el9_3.x86_64            Mon 13 Nov 2023 09:01:22 AM UTC
package kernel-default is not installed`

	rpmKernelsSUSE = `kernel-default-5.14.21-150500.55.65.1.x86_64  Wed 05 Jun 2024 08:14:10 AM UTC
kernel-default-5.14.21-150500.55.52.1.x86_64  Tue 02 Apr 2024 07:55:41 AM UTC
package kernel is not installed
package kernel-core is not installed`

	dpkgKernels = `ii  linux-image-6.1.0-9-amd64
ii  linux-image-6.1.0-26-amd64
ii  linux-image-6.1.0-26-amd64-dbg
ii  linux-image-6.1.0-25-cloud-amd64
rc  linux-image-6.1.0-27-amd64
ii  linux-image-amd64`
)

func rebootRow(t *testing.T, s *check.Section) (check.Row, bool) {
	t.Helper()
	r, ok := checktest.Rows(s)["Reboot Required"]
	return r, ok
}

func mustRow(t *testing.T, s *check.Section, value string, st check.Status, detail string) {
	t.Helper()
	r, ok := rebootRow(t, s)
	if !ok || r.Value != value || r.Status != st || r.Detail != detail {
		t.Fatalf("Reboot Required = %+v (present %v), want %q %v %q", r, ok, value, st, detail)
	}
}

const yesToday = "Yes — since 2026-01-05"

// ── apt ─────────────────────────────────────────────────────────────────────

func TestAptRebootRequiredFileListsThePackages(t *testing.T) {
	e := newEnv(t, "apt-get", nil)
	e.Fake.File("/var/run/reboot-required", "*** System restart required ***\n")
	e.Fake.File("/var/run/reboot-required.pkgs", "linux-image-6.1.0-26-amd64\nlibc6\nlinux-image-6.1.0-26-amd64\n")
	s := e.Run(Check{})
	mustRow(t, s, yesToday, check.Caution, "updated since boot: linux-image-6.1.0-26-amd64, libc6")
	if got := checktest.Facts(s)["reboot_required"]; got.Value != "yes" || got.Status != check.Caution {
		t.Fatalf("fact = %+v", got)
	}
	// The default waits 14 days before anyone is alerted.
	checktest.NoAlerts(t, s)
	var since string
	if !e.Loaded(t, rebootStateKey, &since) || since != "2026-01-05T07:00:00Z" {
		t.Fatalf("reboot_since = %q", since)
	}
}

func TestAptWithoutTheFileComparesKernels(t *testing.T) {
	// Plain Debian writes reboot-required only with unattended-upgrades;
	// its absence alone is not a "no".
	e := newEnv(t, "apt-get", nil)
	e.Fake.Expect("uname -r", "6.1.0-9-amd64")
	e.Fake.Expect("dpkg-query", dpkgKernels)
	mustRow(t, e.Run(Check{}), yesToday, check.Caution, "kernel 6.1.0-26-amd64 installed, 6.1.0-9-amd64 running")
}

func TestAptRunningTheNewestKernelIsNo(t *testing.T) {
	e := newEnv(t, "apt-get", nil)
	e.Fake.Expect("uname -r", "6.1.0-26-amd64")
	e.Fake.Expect("dpkg-query", dpkgKernels)
	s := e.Run(Check{})
	mustRow(t, s, "No", check.OK, "")
	if _, ok := checktest.Facts(s)["reboot_required"]; ok {
		t.Fatal("fact set without a pending reboot")
	}
}

func TestAContainerHasNoRebootRow(t *testing.T) {
	// No kernel package, a foreign running kernel: nothing to compare.
	for _, pm := range []string{"apt-get", "dnf", "yum", "zypper"} {
		e := newEnv(t, pm, nil)
		e.Fake.Expect("uname -r", "6.6.87.2-microsoft-standard-WSL2")
		e.Fake.Expect("dpkg-query", "").Code(1)
		e.Fake.Expect("rpm -q", "package kernel is not installed\npackage kernel-core is not installed").Code(3)
		e.Fake.Expect("zypper --non-interactive needs-rebooting", "").Code(1)
		s := e.Run(Check{})
		if r, ok := rebootRow(t, s); ok {
			t.Errorf("%s: %+v", pm, r)
		}
		if len(s.Facts) != 1 { // only "updates"
			t.Errorf("%s: facts %+v", pm, s.Facts)
		}
	}
}

// ── dnf / yum ───────────────────────────────────────────────────────────────

func TestNeedsRestartingExit1IsYes(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	e.Fake.Tool("needs-restarting")
	e.Fake.Expect("needs-restarting -r", dnfRebootNeeded).Code(1)
	mustRow(t, e.Run(Check{}), yesToday, check.Caution, "updated since boot: glibc, kernel, systemd")
}

func TestNeedsRestartingOnRHEL7(t *testing.T) {
	e := newEnv(t, "yum", nil)
	e.Fake.Tool("needs-restarting")
	e.Fake.Expect("needs-restarting -r", yumRebootNeeded).Code(1)
	mustRow(t, e.Run(Check{}), yesToday, check.Caution, "updated since boot: kernel")
}

func TestNeedsRestartingExit0IsNo(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	e.Fake.Tool("needs-restarting")
	e.Fake.Expect("needs-restarting -r", dnfNoReboot)
	mustRow(t, e.Run(Check{}), "No", check.OK, "")
	if e.Fake.Ran("rpm -q") {
		t.Fatal("the first answer should win")
	}
}

func TestTheDnfPluginIsTriedWithoutTheCommand(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	e.Fake.Expect("dnf needs-restarting -r", dnfRebootNeeded).Code(1)
	mustRow(t, e.Run(Check{}), yesToday, check.Caution, "updated since boot: glibc, kernel, systemd")
}

func TestAMissingDnfPluginIsNotAYes(t *testing.T) {
	// It exits 1 too, which would read as "reboot required".
	e := newEnv(t, "dnf", nil)
	e.Fake.Expect("dnf needs-restarting -r", "").Code(1).Stderr("No such command: needs-restarting.")
	e.Fake.Expect("uname -r", "5.14.0-427.13.1.el9_4.x86_64")
	e.Fake.Expect("rpm -q --last", rpmKernelsEL9).Code(1)
	mustRow(t, e.Run(Check{}), "No", check.OK, "")
}

func TestAnUnexpectedExitFallsThroughToTheKernels(t *testing.T) {
	e := newEnv(t, "yum", nil)
	e.Fake.Tool("needs-restarting")
	e.Fake.Expect("needs-restarting -r", "Error: rpmdb open failed\nreboot").Code(2)
	e.Fake.Expect("uname -r", "5.14.0-362.8.1.el9_3.x86_64")
	e.Fake.Expect("rpm -q --last", rpmKernelsEL9).Code(1)
	mustRow(t, e.Run(Check{}), yesToday, check.Caution,
		"kernel 5.14.0-427.13.1.el9_4.x86_64 installed, 5.14.0-362.8.1.el9_3.x86_64 running")
}

func TestRPMKernelComparison(t *testing.T) {
	for _, c := range []struct {
		running, value string
	}{
		{"5.14.0-427.13.1.el9_4.x86_64", "No"},
		{"5.14.0-362.8.1.el9_3.x86_64", yesToday},
		{"6.9.0-custom", ""}, // not an installed kernel: no answer
	} {
		e := newEnv(t, "dnf", nil)
		e.Fake.Expect("uname -r", c.running)
		e.Fake.Expect("rpm -q --last", rpmKernelsEL9).Code(1)
		r, ok := rebootRow(t, e.Run(Check{}))
		if r.Value != c.value || ok != (c.value != "") {
			t.Errorf("%s: %+v", c.running, r)
		}
	}
}

// ── zypper ──────────────────────────────────────────────────────────────────

func TestZypperNeedsRebooting(t *testing.T) {
	for code, want := range map[int]string{102: yesToday, 0: "No"} {
		e := newEnv(t, "zypper", nil)
		e.Fake.Expect("zypper --non-interactive needs-rebooting", "").Code(code)
		r, _ := rebootRow(t, e.Run(Check{}))
		if r.Value != want {
			t.Errorf("exit %d: %+v", code, r)
		}
	}
}

func TestZypperRebootNeededFile(t *testing.T) {
	e := newEnv(t, "zypper", nil)
	e.Fake.File("/run/reboot-needed", "")
	mustRow(t, e.Run(Check{}), yesToday, check.Caution, "")
}

func TestAnOldZypperFallsThroughToTheSUSEKernelNames(t *testing.T) {
	for running, want := range map[string]string{
		"5.14.21-150500.55.65-default": "No",
		"5.14.21-150500.55.52-default": yesToday,
	} {
		e := newEnv(t, "zypper", nil)
		e.Fake.Expect("zypper --non-interactive needs-rebooting", "").Code(1).Stderr("Unknown command 'needs-rebooting'")
		e.Fake.Expect("uname -r", running)
		e.Fake.Expect("rpm -q --last", rpmKernelsSUSE).Code(2)
		r, _ := rebootRow(t, e.Run(Check{}))
		if r.Value != want {
			t.Errorf("%s: %+v", running, r)
		}
	}
}

// ── age, state, config ──────────────────────────────────────────────────────

func pendingFor(t *testing.T, cfg *Config, age time.Duration) (*checktest.Env, *check.Section) {
	e := newEnv(t, "apt-get", cfg)
	e.Fake.File("/var/run/reboot-required", "")
	e.Seed(t, rebootStateKey, checktest.Now.Add(-age).Format(time.RFC3339))
	return e, e.Run(Check{})
}

func TestARebootAlertsOnlyOnceItHasWaitedTooLong(t *testing.T) {
	_, s := pendingFor(t, nil, 24*time.Hour)
	checktest.NoAlerts(t, s)
	mustRow(t, s, "Yes — since 2026-01-04", check.Caution, "")

	_, s = pendingFor(t, nil, 14*24*time.Hour)
	checktest.NoAlerts(t, s)

	_, s = pendingFor(t, nil, 15*24*time.Hour)
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"Reboot pending for more than 14 days"}) {
		t.Fatalf("alerts %q", got)
	}
	mustRow(t, s, "Yes — since 2025-12-21", check.Caution, "")
}

func TestTheAlertUsesTheConfiguredDays(t *testing.T) {
	_, s := pendingFor(t, &Config{Toggle: check.On, RebootCautionDays: 3}, 4*24*time.Hour)
	if !checktest.HasAlertContaining(s, "more than 3 days") {
		t.Fatalf("alerts %q", s.AlertMsgs())
	}
}

func TestZeroDaysNeverAlerts(t *testing.T) {
	_, s := pendingFor(t, &Config{Toggle: check.On}, 400*24*time.Hour)
	checktest.NoAlerts(t, s)
	mustRow(t, s, "Yes — since 2024-12-01", check.Caution, "")
}

func TestARebootClearsTheClock(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	e.Seed(t, rebootStateKey, "2025-12-01T00:00:00Z")
	e.Fake.Tool("needs-restarting")
	e.Fake.Expect("needs-restarting -r", dnfNoReboot)
	e.Run(Check{})
	var since string
	if !e.Loaded(t, rebootStateKey, &since) || since != "" {
		t.Fatalf("reboot_since = %q, want cleared", since)
	}
}

func TestAnUnknownAnswerKeepsTheClock(t *testing.T) {
	// One run that cannot tell must not restart the 14 days.
	e := newEnv(t, "dnf", nil)
	e.Seed(t, rebootStateKey, "2025-12-01T00:00:00Z")
	s := e.Run(Check{})
	if _, ok := rebootRow(t, s); ok {
		t.Fatal("a row without an answer")
	}
	var since string
	if e.Loaded(t, rebootStateKey, &since); since != "2025-12-01T00:00:00Z" {
		t.Fatalf("reboot_since = %q", since)
	}
}

func TestACorruptOrFutureStampRestartsTheClock(t *testing.T) {
	for _, stamp := range []string{"yesterday", "2027-01-01T00:00:00Z"} {
		e := newEnv(t, "apt-get", nil)
		e.Fake.File("/var/run/reboot-required", "")
		e.Seed(t, rebootStateKey, stamp)
		mustRow(t, e.Run(Check{}), yesToday, check.Caution, "")
	}
}

func TestTheRebootRowSurvivesUnreachableMirrors(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	e.Fake.Expect("dnf check-update", "").Code(1).Stderr("Error: Failed to download metadata for repo 'baseos'")
	e.Fake.Tool("needs-restarting")
	e.Fake.Expect("needs-restarting -r", dnfRebootNeeded).Code(1)
	s := e.Run(Check{})
	if !slices.Equal(checktest.Labels(s), []string{"Pending Updates", "Reboot Required"}) {
		t.Fatalf("rows %q", checktest.Labels(s))
	}
}

func TestRebootDaysMustNotBeNegative(t *testing.T) {
	if err := (&Config{RebootCautionDays: -1}).Validate(); err == nil || !strings.Contains(err.Error(), "reboot_caution_days") {
		t.Fatalf("err = %v", err)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func TestDebFlavour(t *testing.T) {
	for release, want := range map[string]string{
		"6.1.0-26-amd64":                   "amd64",
		"6.1.0-26-cloud-amd64":             "cloud-amd64",
		"6.8.0-45-generic":                 "generic",
		"amd64":                            "",
		"6.1.0-26-amd64-dbg":               "",
		"6.6.87.2-microsoft-standard-WSL2": "",
	} {
		if got := debFlavour(release); got != want {
			t.Errorf("debFlavour(%q) = %q, want %q", release, got, want)
		}
	}
}

func TestVersionCmp(t *testing.T) {
	for _, c := range [][2]string{
		{"6.1.0-9-amd64", "6.1.0-26-amd64"},
		{"6.1.0-26-amd64", "6.1.0-27-amd64"},
		{"5.15.0-9", "6.1.0-1"},
	} {
		if versionCmp(c[0], c[1]) >= 0 || versionCmp(c[1], c[0]) <= 0 {
			t.Errorf("%s should sort before %s", c[0], c[1])
		}
	}
	if versionCmp("6.1.0-26-amd64", "6.1.0-26-amd64") != 0 {
		t.Error("equal versions")
	}
}
