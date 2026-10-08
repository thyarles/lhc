package kubernetes

import (
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

func events(t *testing.T, e *checktest.Env, lines ...string) *check.Section {
	t.Helper()
	cluster(e, oneNode)
	e.Fake.Expect(eventsCmd, strings.Join(lines, "\n"))
	return e.Run(Check{})
}

func TestRoutineWarningEventsAreACountAtAnyVolume(t *testing.T) {
	// A busy cluster emits hundreds of Unhealthy probe events an hour.
	s := events(t, newEnv(t), "Unhealthy 340 <none>", "BackOff 55 <none>", "FailedScheduling 12 <none>")
	if got := statusOf(t, s, "Warning Events"); got != check.Info {
		t.Fatalf("status %v", got)
	}
	checktest.NoAlerts(t, s)
}

func TestNodeLevelEventsDoAlert(t *testing.T) {
	s := events(t, newEnv(t), "Unhealthy 11 <none>", "SystemOOM 2 <none>")
	if got := statusOf(t, s, "Node-level Events"); got != check.Caution {
		t.Fatalf("status %v", got)
	}
	if !strings.Contains(s.AlertMsgs()[0], "SystemOOM") {
		t.Fatalf("alerts %q", s.AlertMsgs())
	}
}

func TestTheEventWindowIsNeverCalled24h(t *testing.T) {
	// --event-ttl defaults to ONE HOUR on k3s and rke2. Claiming a 24h window
	// would make a cluster look quiet after an incident.
	s := events(t, newEnv(t), "Unhealthy 3 <none>")
	v := checktest.Rows(s)["Warning Events"].Value
	if strings.Contains(v, "24h") || !strings.Contains(v, "retained") {
		t.Fatalf("value %q", v)
	}
}

func TestEventWindowComesFromTheOldestTimestamp(t *testing.T) {
	// checktest.Now is 07:00 UTC; the oldest event is 45 minutes before it.
	// '<none>' in COUNT counts as one.
	s := events(t, newEnv(t),
		"BackOff 2 2026-01-05T06:50:00Z",
		"Unhealthy <none> 2026-01-05T06:15:00Z",
		"BackOff 3 <none>")
	if v := checktest.Rows(s)["Warning Events"].Value; v != "6 in the ~45m retained: BackOff ×5, Unhealthy ×1" {
		t.Fatalf("value %q", v)
	}
}
