package kubernetes

import (
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

func TestNotReadyOnThisHostIsUnhealthy(t *testing.T) {
	e := newEnv(t)
	cluster(e, "k3s-01||v1.30.5|Ready=False,|k3s-01,10.0.0.5,")
	s := e.Run(Check{})
	if got := statusOf(t, s, "k3s-01 (this host)"); got != check.Unhealthy {
		t.Fatalf("status %v", got)
	}
	if len(s.Alerts) == 0 || !strings.Contains(s.Alerts[0].Msg, "NotReady") || s.Alerts[0].Status != check.Unhealthy {
		t.Fatalf("alerts: %+v", s.Alerts)
	}
}

func TestNotReadyElsewhereIsOnlyCaution(t *testing.T) {
	// Another node runs its own health check; do not page this host at full
	// severity for someone else's machine.
	e := newEnv(t)
	e.Fake.Expect("--field-selector=spec.nodeName=", "")
	cluster(e, "k3s-01||v1.30.5|Ready=True,|k3s-01,10.0.0.5,\nk3s-02||v1.30.5|Ready=Unknown,|k3s-02,10.0.0.6,")
	s := e.Run(Check{})
	if got := statusOf(t, s, "k3s-02"); got != check.Caution {
		t.Fatalf("k3s-02: %v", got)
	}
	if s.Status != check.Caution {
		t.Fatalf("section: %v", s.Status)
	}
}

func TestANodeCordonedOnBothRunsIsNotNews(t *testing.T) {
	// Cordoning is deliberate maintenance. It must not be a permanent yellow.
	e := newEnv(t)
	e.Seed(t, "cordoned", []string{"k3s-01"})
	cluster(e, "k3s-01|true|v1.30.5|Ready=True,|k3s-01,10.0.0.5,")
	s := e.Run(Check{})
	if got := statusOf(t, s, "k3s-01 (this host)"); got != check.Info {
		t.Fatalf("status %v", got)
	}
	checktest.NoAlerts(t, s)
}

func TestANewlyCordonedNodeIsReportedOnce(t *testing.T) {
	e := newEnv(t)
	cluster(e, "k3s-01|true|v1.30.5|Ready=True,|k3s-01,10.0.0.5,")
	s := e.Run(Check{})
	if got := statusOf(t, s, "k3s-01 (this host)"); got != check.Caution {
		t.Fatalf("status %v", got)
	}
	if !strings.Contains(s.AlertMsgs()[0], "cordoned") {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
	var saved []string
	if !e.Loaded(t, "cordoned", &saved) || !slices.Equal(saved, []string{"k3s-01"}) {
		t.Fatalf("saved %q", saved)
	}
}

func TestDiskPressureIsCautionAndGrouped(t *testing.T) {
	e := newEnv(t)
	cluster(e, "k3s-01||v1.30.5|DiskPressure=True,Ready=True,|k3s-01,10.0.0.5,")
	s := e.Run(Check{})
	if got := statusOf(t, s, "k3s-01 (this host)"); got != check.Caution {
		t.Fatalf("status %v", got)
	}
	alertCount(t, s, 1)
	if !strings.Contains(s.Alerts[0].Msg, "DiskPressure") {
		t.Fatalf("alert: %q", s.Alerts[0].Msg)
	}
}

func TestPressureUnknownOnADeadNodeIsOneAlertNotFive(t *testing.T) {
	// A kubelet that stopped reporting sets every condition to Unknown. That
	// is one fact, not five.
	e := newEnv(t)
	cluster(e, "k3s-01||v1.30.5|MemoryPressure=Unknown,DiskPressure=Unknown,"+
		"PIDPressure=Unknown,Ready=Unknown,|k3s-01,10.0.0.5,")
	alertCount(t, e.Run(Check{}), 1)
}
