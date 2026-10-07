package updates

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

func newEnv(t *testing.T, pm string, cfg *Config) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	e.Host.PkgManager = pm
	return e
}

func rowsWith(s *check.Section, label string) []string {
	var out []string
	for _, r := range s.Rows {
		if r.Label == label {
			out = append(out, r.Value)
		}
	}
	return out
}

// aptSim is an `apt-get -s upgrade` with 82 upgrades, 45 of them from the
// -security archive. Every package also has a Conf line, which is what the
// old case-insensitive grep double-counted.
func aptSim() string {
	lines := []string{
		"Reading package lists...",
		"Building dependency tree...",
		"The following packages will be upgraded:",
		"82 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}
	for i := range 82 {
		archive := "Ubuntu:22.04/jammy-updates [amd64]"
		if i < 45 {
			archive = "Ubuntu:22.04/jammy-updates, Ubuntu:22.04/jammy-security [amd64]"
		}
		lines = append(lines, fmt.Sprintf("Inst pkg%d [1.0-1] (1.0-2 %s)", i, archive))
	}
	for i := range 82 {
		lines = append(lines, fmt.Sprintf("Conf pkg%d (1.0-2 Ubuntu:22.04/jammy-updates, Ubuntu:22.04/jammy-security [amd64])", i))
	}
	return strings.Join(lines, "\n")
}

func rpmUpdates(n int) string {
	var lines []string
	for i := range n {
		lines = append(lines, fmt.Sprintf("pkg%d.x86_64 1.0 base", i))
	}
	return strings.Join(lines, "\n")
}

func TestAptSecurityCountIgnoresConfLines(t *testing.T) {
	// The old grep matched the Inst and the Conf line of each package,
	// reporting exactly twice the real number (90 instead of 45).
	e := newEnv(t, "apt-get", nil)
	e.Fake.Expect("apt-get -s upgrade", aptSim())
	s := e.Run(Check{})
	if got := rowsWith(s, "Security Updates"); !slices.Equal(got, []string{"45"}) {
		t.Fatalf("Security Updates = %q", got)
	}
	if got := checktest.Rows(s)["Security Updates"].Status; got != check.Info {
		t.Fatalf("status = %v", got)
	}
	if got := checktest.Rows(s)["Pending Updates"].Value; got != "82" {
		t.Fatalf("Pending Updates = %q", got)
	}
}

func TestAptRefreshesThenSimulatesOnce(t *testing.T) {
	e := newEnv(t, "apt-get", nil)
	e.Fake.Expect("apt-get -s upgrade", aptSim())
	e.Run(Check{})
	calls := e.Fake.Calls()
	if !slices.Equal(calls, []string{"apt-get update -qq", "apt-get -s upgrade"}) {
		t.Fatalf("calls: %q", calls)
	}
}

func TestPendingUpdatesDoNotAlertByDefault(t *testing.T) {
	e := newEnv(t, "apt-get", nil)
	e.Fake.Expect("apt-get -s upgrade", aptSim())
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if s.Status != check.Info {
		t.Fatalf("status = %v", s.Status)
	}
}

func TestPendingUpdatesAlertOnceAThresholdIsConfigured(t *testing.T) {
	e := newEnv(t, "apt-get", &Config{Toggle: check.On, Caution: 50, SecurityCaution: 10})
	e.Fake.Expect("apt-get -s upgrade", aptSim())
	s := e.Run(Check{})
	want := []string{"82 pending package updates", "45 pending security updates"}
	if got := s.AlertMsgs(); !slices.Equal(got, want) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestRPMPendingUpdatesDoNotAlertByDefault(t *testing.T) {
	for _, pm := range []string{"dnf", "yum"} {
		t.Run(pm, func(t *testing.T) {
			e := newEnv(t, pm, nil)
			// check-update exits 100 when there are updates.
			e.Fake.Expect(pm+" check-update", rpmUpdates(40)).Code(100)
			s := e.Run(Check{})
			checktest.NoAlerts(t, s)
			if got := checktest.Rows(s)["Pending Updates"]; got.Status != check.Info || got.Value != "40" {
				t.Fatalf("Pending Updates row: %+v", got)
			}
		})
	}
}

func TestRPMPendingUpdatesAlertOnceConfigured(t *testing.T) {
	for _, pm := range []string{"dnf", "yum"} {
		t.Run(pm, func(t *testing.T) {
			e := newEnv(t, pm, &Config{Toggle: check.On, Caution: 30})
			e.Fake.Expect(pm+" check-update", rpmUpdates(40)).Code(100)
			s := e.Run(Check{})
			if got := s.AlertMsgs(); !slices.Equal(got, []string{"40 pending package updates"}) {
				t.Fatalf("alerts: %q", got)
			}
		})
	}
}

func TestDnfNoiseLinesAreNotUpdates(t *testing.T) {
	out := `Last metadata expiration check: 0:12:01 ago on Mon 05 Jan 2026 06:48:00 AM UTC.

kernel.x86_64                     5.14.0-427.16.1.el9_4      baseos
openssl.x86_64                    1:3.0.7-27.el9_4           baseos
Security: kernel-core-5.14.0-427.13.1.el9_4.x86_64 is an installed security update
Obsoleting Packages
grub2-tools.x86_64                1:2.06-80.el9              baseos
    grub2-tools.x86_64            1:2.06-77.el9              @baseos`
	if got := countRPM(out); got != 3 {
		t.Fatalf("countRPM = %d, want 3", got)
	}
}

func TestNoUpdatesIsOK(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Pending Updates"]; got.Status != check.OK || got.Value != "0" {
		t.Fatalf("Pending Updates row: %+v", got)
	}
}

func TestUnreachableRepositoriesAreNotReportedAsZero(t *testing.T) {
	e := newEnv(t, "dnf", nil)
	e.Fake.Expect("dnf check-update", "").Code(1).Stderr("Error: Failed to download metadata for repo 'baseos'")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	row := checktest.Rows(s)["Pending Updates"]
	if row.Value != "Could not be determined" || !strings.Contains(row.Detail, "Failed to download metadata") {
		t.Fatalf("Pending Updates row: %+v", row)
	}
}

func TestUpdatesWithNoPackageManagerIsInformational(t *testing.T) {
	s := newEnv(t, "", nil).Run(Check{})
	checktest.NoAlerts(t, s)
	if s.Status != check.Info {
		t.Fatalf("status = %v", s.Status)
	}
}

const zypperUpdates = `S | Repository                          | Name        | Current Version        | Available Version       | Arch
--+-------------------------------------+-------------+------------------------+-------------------------+-------
v | SLE-Module-Basesystem15-SP5-Updates | libopenssl3 | 3.0.8-150500.5.20.1    | 3.0.8-150500.5.27.1     | x86_64
v | SLE-Module-Basesystem15-SP5-Updates | openssl-3   | 3.0.8-150500.5.20.1    | 3.0.8-150500.5.27.1     | x86_64
v | SLE-Module-Basesystem15-SP5-Updates | vim         | 9.0.2103-150500.20.6.1 | 9.1.0330-150500.20.12.1 | x86_64`

const zypperPatches = `Repository                          | Name                                        | Category | Severity  | Interactive | Status | Summary
------------------------------------+---------------------------------------------+----------+-----------+-------------+--------+------------------------------
SLE-Module-Basesystem15-SP5-Updates | SUSE-SLE-Module-Basesystem-15-SP5-2024-1234 | security | important | ---         | needed | Security update for openssl-3
SLE-Module-Basesystem15-SP5-Updates | SUSE-SLE-Module-Basesystem-15-SP5-2024-1301 | security | moderate  | ---         | needed | Security update for vim

Found 2 applicable patches:
2 patches needed (2 security patches)`

func TestZypperCountsUpdatesAndSecurityPatches(t *testing.T) {
	e := newEnv(t, "zypper", nil)
	e.Fake.Expect("zypper --non-interactive --quiet list-updates", zypperUpdates)
	e.Fake.Expect("zypper --non-interactive --quiet list-patches --category security", zypperPatches)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	rows := checktest.Rows(s)
	if rows["Pending Updates"].Value != "3" || rows["Security Updates"].Value != "2" {
		t.Fatalf("rows: %+v", s.Rows)
	}
}

func TestZypperThresholdsAlert(t *testing.T) {
	e := newEnv(t, "zypper", &Config{Toggle: check.On, Caution: 3, SecurityCaution: 1})
	e.Fake.Expect("list-updates", zypperUpdates)
	e.Fake.Expect("list-patches", zypperPatches)
	want := []string{"3 pending package updates", "2 pending security updates"}
	if got := e.Run(Check{}).AlertMsgs(); !slices.Equal(got, want) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestZypperNothingPending(t *testing.T) {
	e := newEnv(t, "zypper", nil)
	e.Fake.Expect("list-updates", "No updates found.")
	e.Fake.Expect("list-patches", "No updates found.")
	s := e.Run(Check{})
	if s.Status != check.OK || slices.Contains(checktest.Labels(s), "Security Updates") {
		t.Fatalf("section: %+v", s.Rows)
	}
}

func TestZypperLockedIsNotReportedAsZero(t *testing.T) {
	e := newEnv(t, "zypper", nil)
	e.Fake.Expect("list-updates", "").Code(7).Stderr("System management is locked by the application with pid 1234 (zypper).")
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Pending Updates"].Value; got != "Could not be determined" {
		t.Fatalf("Pending Updates = %q", got)
	}
}

func TestValidate(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.SecurityCaution = -1
	if c.Validate() == nil {
		t.Fatal("negative threshold accepted")
	}
}
