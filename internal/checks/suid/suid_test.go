package suid

import (
	"slices"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

const findOut = `/usr/bin/sudo
/usr/bin/passwd
/usr/bin/chsh
/usr/bin/mount`

var baseline = []string{"/usr/bin/chsh", "/usr/bin/mount", "/usr/bin/passwd", "/usr/bin/sudo"}

func TestFindUsesThePortablePermSyntax(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Run(Check{})
	if !e.Fake.Ran("find / -xdev -perm -4000 -type f") {
		t.Fatalf("calls: %q", e.Fake.Calls())
	}
}

func TestFirstRunRecordsTheBaseline(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("find /", findOut)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["Baseline"].Value; got != "4 SUID files recorded (first run)" {
		t.Fatalf("Baseline = %q", got)
	}
	var saved []string
	if !e.Loaded(t, stateKey, &saved) || !slices.Equal(saved, baseline) {
		t.Fatalf("saved = %q", saved)
	}
}

func TestUnchangedIsOK(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Seed(t, stateKey, baseline)
	e.Fake.Expect("find /", findOut)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["SUID Changes"]; got.Status != check.OK || got.Value != "None (4 files, unchanged)" {
		t.Fatalf("SUID Changes row: %+v", got)
	}
}

func TestANewSUIDFileIsUnhealthy(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Seed(t, stateKey, baseline)
	e.Fake.Expect("find /", findOut+"\n/tmp/.hidden/bash")
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"New SUID file: /tmp/.hidden/bash"}) {
		t.Fatalf("alerts: %q", got)
	}
	if s.Alerts[0].Status != check.Unhealthy || s.Status != check.Unhealthy {
		t.Fatalf("alert %v, section %v", s.Alerts[0].Status, s.Status)
	}
	if got := checktest.Rows(s)["NEW"]; got.Value != "/tmp/.hidden/bash" || got.Status != check.Unhealthy {
		t.Fatalf("NEW row: %+v", got)
	}
}

func TestARemovedSUIDFileIsInformational(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Seed(t, stateKey, baseline)
	e.Fake.Expect("find /", "/usr/bin/sudo\n/usr/bin/passwd\n/usr/bin/mount")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["GONE"]; got.Value != "/usr/bin/chsh" || got.Status != check.Info {
		t.Fatalf("GONE row: %+v", got)
	}
}

func TestAScanThatDidNotFinishKeepsTheBaseline(t *testing.T) {
	// A timed-out find returns a partial list; saving it would make every
	// file it missed look new tomorrow.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Seed(t, stateKey, baseline)
	e.Fake.Expect("find /", "/usr/bin/sudo").Code(-1).Stderr("timeout")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["SUID Scan"]; got.Status != check.Caution {
		t.Fatalf("rows: %+v", s.Rows)
	}
	var saved []string
	if !e.Loaded(t, stateKey, &saved) || !slices.Equal(saved, baseline) {
		t.Fatalf("baseline overwritten: %q", saved)
	}
}

func TestPermissionDeniedSomewhereIsNotAFailure(t *testing.T) {
	// find exits 1 when one directory is unreadable but still lists the rest.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Seed(t, stateKey, baseline)
	e.Fake.Expect("find /", findOut).Code(1).Stderr("find: '/proc/1/fd': Permission denied")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if checktest.Rows(s)["SUID Changes"].Status != check.OK {
		t.Fatalf("rows: %v", checktest.Labels(s))
	}
}
