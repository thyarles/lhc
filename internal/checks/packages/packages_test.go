package packages

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

const rpmOut = `bash-5.1.8-9.el9
openssl-3.0.7-27.el9
kernel-core-5.14.0-427.13.1.el9_4`

const dpkgOut = `Desired=Unknown/Install/Remove/Purge/Hold
| Status=Not/Inst/Conf-files/Unpacked/halF-conf/Half-inst/trig-aWait/Trig-pend
|/ Err?=(none)/Reinst-required (Status,Err: uppercase=bad)
||/ Name           Version         Architecture Description
+++-==============-===============-============-=================================
ii  bash           5.2.21-2ubuntu4 amd64        GNU Bourne Again SHell
rc  oldpkg         1.0-1           amd64        removed, config files remain
ii  libc6:amd64    2.39-0ubuntu8.3 amd64        GNU C Library: Shared libraries`

const dnfHistory = `ID     | Command line             | Date and time    | Action(s)      | Altered
--------------------------------------------------------------------------------
    42 | install htop             | 2026-01-04 10:00 | Install        |    1
    41 | update -y                | 2025-12-20 03:00 | Upgrade        |   57
    40 | install vim              | 2025-12-01 09:00 | Install        |    2
    39 | install tmux             | 2025-11-01 09:00 | Install        |    1`

func newEnv(t *testing.T, pm string) *checktest.Env {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Host.PkgManager = pm
	return e
}

func TestRPMFamiliesUseTheSameQuery(t *testing.T) {
	for _, pm := range []string{"dnf", "yum", "zypper"} {
		t.Run(pm, func(t *testing.T) {
			e := newEnv(t, pm)
			e.Fake.Expect("rpm -qa", rpmOut)
			s := e.Run(Check{})
			if !e.Fake.Ran(`rpm -qa --qf %{NAME}-%{VERSION}-%{RELEASE}\n`) {
				t.Fatalf("calls: %q", e.Fake.Calls())
			}
			if got := checktest.Rows(s)["Baseline"].Value; got != "3 packages recorded (first run)" {
				t.Fatalf("Baseline = %q", got)
			}
		})
	}
}

func TestZypperHasNoTransactionHistory(t *testing.T) {
	e := newEnv(t, "zypper")
	e.Fake.Expect("rpm -qa", rpmOut)
	s := e.Run(Check{})
	if e.Fake.Ran("history") || slices.Contains(checktest.Labels(s), "Recent Transaction History") {
		t.Fatalf("history on zypper: %q", e.Fake.Calls())
	}
}

func TestDpkgListsOnlyInstalledPackages(t *testing.T) {
	e := newEnv(t, "apt-get")
	e.Fake.Expect("dpkg -l", dpkgOut)
	e.Run(Check{})
	var saved []string
	if !e.Loaded(t, stateKey, &saved) {
		t.Fatal("baseline not saved")
	}
	if want := []string{"bash-5.2.21-2ubuntu4", "libc6:amd64-2.39-0ubuntu8.3"}; !slices.Equal(saved, want) {
		t.Fatalf("saved = %q, want %q", saved, want)
	}
}

func TestFirstRunRecordsTheBaselineWithoutAlerting(t *testing.T) {
	e := newEnv(t, "dnf")
	e.Fake.Expect("rpm -qa", rpmOut)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	var saved []string
	if !e.Loaded(t, stateKey, &saved) || len(saved) != 3 || saved[0] != "bash-5.1.8-9.el9" {
		t.Fatalf("saved = %q", saved)
	}
}

func TestUnchangedPackagesAreOK(t *testing.T) {
	e := newEnv(t, "dnf")
	e.Fake.Expect("rpm -qa", rpmOut)
	e.Run(Check{})
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Package Changes"]; got.Status != check.OK || got.Value != "None (3 packages)" {
		t.Fatalf("Package Changes row: %+v", got)
	}
}

func TestInstallsAndRemovalsAreAnAuditTrailNotAnAlert(t *testing.T) {
	// Package churn is expected with unattended upgrades: report, never page.
	e := newEnv(t, "dnf")
	e.Seed(t, stateKey, []string{"bash-5.1.8-9.el9", "openssl-3.0.7-25.el9"})
	e.Fake.Expect("rpm -qa", rpmOut)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if len(checktest.Flagged(s)) != 0 {
		t.Fatalf("flagged rows: %+v", checktest.Flagged(s))
	}
	var inst, rem []string
	for _, r := range s.Rows {
		switch r.Label {
		case "INSTALLED":
			inst = append(inst, r.Value)
		case "REMOVED":
			rem = append(rem, r.Value)
		}
	}
	if !slices.Equal(inst, []string{"kernel-core-5.14.0-427.13.1.el9_4", "openssl-3.0.7-27.el9"}) {
		t.Errorf("INSTALLED = %q", inst)
	}
	if !slices.Equal(rem, []string{"openssl-3.0.7-25.el9"}) {
		t.Errorf("REMOVED = %q", rem)
	}
}

func TestLongChangeListsAreCapped(t *testing.T) {
	e := newEnv(t, "dnf")
	e.Seed(t, stateKey, []string{"bash-5.1.8-9.el9"})
	var lines []string
	for i := range 25 {
		lines = append(lines, fmt.Sprintf("pkg%02d-1.0-1.el9", i))
	}
	e.Fake.Expect("rpm -qa", strings.Join(lines, "\n"))
	s := e.Run(Check{})
	if got := checktest.Rows(s)["..."].Value; got != "and 5 more" {
		t.Fatalf("cap row = %q", got)
	}
}

func TestDnfTransactionHistoryShowsFiveLines(t *testing.T) {
	e := newEnv(t, "dnf")
	e.Fake.Expect("rpm -qa", rpmOut)
	e.Fake.Expect("dnf history list", dnfHistory)
	s := e.Run(Check{})
	labels := checktest.Labels(s)
	i := slices.Index(labels, "Recent Transaction History")
	if i < 0 || len(labels)-i-1 != 5 {
		t.Fatalf("labels: %q", labels)
	}
	if !strings.Contains(s.Rows[len(s.Rows)-1].Value, "install vim") {
		t.Fatalf("last row: %+v", s.Rows[len(s.Rows)-1])
	}
}

func TestEmptyPackageListKeepsTheBaseline(t *testing.T) {
	// A failed rpm query must not make tomorrow report every package as new.
	e := newEnv(t, "yum")
	e.Seed(t, stateKey, []string{"bash-5.1.8-9.el9"})
	s := e.Run(Check{})
	if checktest.Rows(s)["Installed Packages"].Status != check.Caution {
		t.Fatalf("rows: %v", checktest.Labels(s))
	}
	var saved []string
	if !e.Loaded(t, stateKey, &saved) || !slices.Equal(saved, []string{"bash-5.1.8-9.el9"}) {
		t.Fatalf("baseline overwritten: %q", saved)
	}
}

func TestNoPackageManagerIsInformational(t *testing.T) {
	s := newEnv(t, "").Run(Check{})
	checktest.NoAlerts(t, s)
	if s.Status != check.Info || checktest.Rows(s)["Package Manager"].Value != "Not detected" {
		t.Fatalf("section: %+v", s)
	}
}
