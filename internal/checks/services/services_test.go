package services

import (
	"slices"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

func newEnv(t *testing.T) *checktest.Env {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("systemctl").Dir("/run/systemd/system")
	return e
}

func TestUnitCaughtMidStartupIsNotStuck(t *testing.T) {
	e := newEnv(t)
	e.Fake.Expect("--state=activating", "wsl-pro.service loaded activating start")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["wsl-pro.service"]; got.Status != check.Info || got.Value != "activating (starting up)" {
		t.Fatalf("row %+v", got)
	}
	var saved []string
	if !e.Loaded(t, activatingKey, &saved) || !slices.Equal(saved, []string{"wsl-pro.service"}) {
		t.Fatalf("saved %v", saved)
	}
}

func TestUnitStillActivatingOnTheNextRunIsStuck(t *testing.T) {
	e := newEnv(t)
	e.Seed(t, activatingKey, []string{"wsl-pro.service"})
	e.Fake.Expect("--state=activating", "wsl-pro.service loaded activating start")
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"Service wsl-pro.service stuck activating"}) {
		t.Fatalf("alerts %q", got)
	}
	if s.Alerts[0].Status != check.Caution || checktest.Rows(s)["wsl-pro.service"].Status != check.Caution {
		t.Fatalf("not CAUTION: %+v", s.Rows)
	}
}

func TestFailedUnitsAlwaysAlert(t *testing.T) {
	e := newEnv(t)
	e.Fake.Expect("--state=failed", "nginx.service loaded failed failed A high performance web server")
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"Service nginx.service failed"}) {
		t.Fatalf("alerts %q", got)
	}
	row := checktest.Rows(s)["nginx.service"]
	if row.Status != check.Unhealthy || row.Value != "A high performance web server" {
		t.Fatalf("row %+v", row)
	}
	if _, ok := checktest.Rows(s)["Failed Services"]; ok {
		t.Fatal(`"Failed Services: None" shown alongside a failed unit`)
	}
}

func TestFailedUnitMarkerIsNotTheUnitName(t *testing.T) {
	// systemd 246+ prefixes a failed unit with "●" ("*" under LC_ALL=C).
	for _, marker := range []string{"● ", "* "} {
		e := newEnv(t)
		e.Fake.Expect("--state=failed", marker+"nginx.service loaded failed failed nginx")
		if got := e.Run(Check{}).AlertMsgs(); !slices.Equal(got, []string{"Service nginx.service failed"}) {
			t.Errorf("marker %q: alerts %q", marker, got)
		}
	}
}

func TestFailedUnitWithoutDescription(t *testing.T) {
	e := newEnv(t)
	e.Fake.Expect("--state=failed", "odd.service loaded failed")
	if v := checktest.Rows(e.Run(Check{}))["odd.service"].Value; v != "failed" {
		t.Fatalf("value %q", v)
	}
}

func TestNothingFailedIsOneOKRow(t *testing.T) {
	e := newEnv(t)
	s := e.Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"Failed Services"}) {
		t.Fatalf("labels %v", got)
	}
	if s.Status != check.OK {
		t.Fatalf("status %v", s.Status)
	}
}

func TestOnlyFiveActivatingUnitsAreListed(t *testing.T) {
	e := newEnv(t)
	e.Fake.Expect("--state=activating", "a.service x\nb.service x\nc.service x\nd.service x\ne.service x\nf.service x")
	if got := checktest.Labels(e.Run(Check{})); len(got) != 6 || slices.Contains(got, "f.service") {
		t.Fatalf("labels %v", got)
	}
}

func TestStuckStateIsReplacedEachRun(t *testing.T) {
	// A unit that finished starting is forgotten, so a later restart counts
	// as a fresh sighting again.
	e := newEnv(t)
	e.Seed(t, activatingKey, []string{"old.service"})
	checktest.NoAlerts(t, e.Run(Check{}))
	var saved []string
	if !e.Loaded(t, activatingKey, &saved) || len(saved) != 0 {
		t.Fatalf("saved %v", saved)
	}
}

func TestWithoutSystemctlTheSectionIsNotApplicable(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	if s.Applicable || s.Status != check.Info || len(s.Rows) != 1 || s.Rows[0].Value != notSystemd {
		t.Fatalf("section %+v", s)
	}
	if e.Fake.Ran("systemctl") {
		t.Fatal("ran systemctl although it is not installed")
	}
}

func TestSystemctlWithoutSystemdIsNotApplicable(t *testing.T) {
	// Containers and WSL without systemd ship the systemctl binary anyway.
	cases := []struct {
		name   string
		stderr string
		booted bool
	}{
		{"not booted", "System has not been booted with systemd as init system (PID 1). Can't operate.\nFailed to connect to bus: Host is down", true},
		{"no bus and no systemd", "Failed to connect to bus: No such file or directory", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := checktest.NewEnv(t, Check{}, nil)
			e.Fake.Tool("systemctl")
			if c.booted {
				e.Fake.Dir("/run/systemd/system")
			}
			e.Fake.Expect("systemctl", "").Code(1).Stderr(c.stderr)
			s := e.Run(Check{})
			if s.Applicable || s.Status != check.Info || s.Rows[0].Value != notSystemd {
				t.Fatalf("section %+v", s)
			}
			checktest.NoAlerts(t, s)
		})
	}
}

func TestBrokenBusOnASystemdHostIsNotHidden(t *testing.T) {
	e := newEnv(t)
	e.Fake.Expect("systemctl", "").Code(1).Stderr("Failed to connect to bus: Connection refused")
	s := e.Run(Check{})
	row := checktest.Rows(s)["Failed Services"]
	if !s.Applicable || row.Status != check.Info || row.Value != "could not query systemd: Failed to connect to bus: Connection refused" {
		t.Fatalf("section %+v", s)
	}
	checktest.NoAlerts(t, s)
}
