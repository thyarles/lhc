package notify

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc-go/internal/alerts"
	"github.com/thyarles/lhc-go/internal/check"
)

// Who receives which message, and what the subject tells them. The original
// defect was social as much as technical: the broad list got a CAUTION mail
// every day, so the word stopped meaning anything.

var (
	ops  = []string{"ops@example.com"}
	all  = []string{"team@example.com", "manager@example.com"}
	host = "srv01.example.com"
	now  = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)
	disk = check.Alert{Status: check.Caution, Msg: "Disk /var at 91%"}
)

func decision(f func(d *alerts.Decision)) *alerts.Decision {
	d := &alerts.Decision{}
	if f != nil {
		f(d)
	}
	return d
}

func newDisk(d *alerts.Decision) { d.New = []check.Alert{disk}; d.NotifyAll = true }

// ── delivery planning ───────────────────────────────────────────────────────

func TestQuietRunGoesOnlyToTheDailyGroup(t *testing.T) {
	to, isAlert := PlanDelivery(decision(nil), ops, all)
	if !slices.Equal(to, ops) || isAlert {
		t.Fatal(to, isAlert)
	}
}

func TestKnownUnchangedProblemGoesOnlyToTheDailyGroup(t *testing.T) {
	// The whole point: an ongoing CAUTION must not reach everyone again.
	to, isAlert := PlanDelivery(decision(func(d *alerts.Decision) { d.Ongoing = []check.Alert{disk} }), ops, all)
	if !slices.Equal(to, ops) || isAlert {
		t.Fatal(to, isAlert)
	}
}

func TestNewFindingReachesEveryone(t *testing.T) {
	to, isAlert := PlanDelivery(decision(newDisk), ops, all)
	if !isAlert || len(to) != 3 || !slices.Contains(to, ops[0]) || !slices.Contains(to, all[0]) || !slices.Contains(to, all[1]) {
		t.Fatal(to, isAlert)
	}
}

func TestAlertIsOneMessageWithNoDuplicateRecipients(t *testing.T) {
	to, _ := PlanDelivery(decision(newDisk), []string{"ops@example.com", "manager@example.com"}, all)
	seen := map[string]bool{}
	for _, r := range to {
		if seen[r] {
			t.Fatalf("someone would get two copies: %v", to)
		}
		seen[r] = true
	}
}

func TestDailyGroupIsNeverDroppedFromAnAlert(t *testing.T) {
	to, _ := PlanDelivery(decision(newDisk), ops, all)
	if !slices.Contains(to, ops[0]) {
		t.Fatal(to)
	}
}

func TestFindingsDegradeToAHeartbeatWithoutABroadList(t *testing.T) {
	// Better the ops team sees it than nobody does.
	to, isAlert := PlanDelivery(decision(newDisk), ops, nil)
	if !slices.Equal(to, ops) || isAlert {
		t.Fatal(to, isAlert)
	}
}

func TestNothingIsSentWithNoRecipients(t *testing.T) {
	to, isAlert := PlanDelivery(decision(newDisk), nil, nil)
	if len(to) != 0 || isAlert {
		t.Fatal(to, isAlert)
	}
}

func TestPlanDoesNotMutateTheCallerLists(t *testing.T) {
	daily, broad := slices.Clone(ops), slices.Clone(all)
	PlanDelivery(decision(newDisk), daily, broad)
	if !slices.Equal(daily, ops) || !slices.Equal(broad, all) {
		t.Fatal(daily, broad)
	}
}

// ── subject lines ───────────────────────────────────────────────────────────

func TestAlertSubjectIsTaggedForAction(t *testing.T) {
	s := Subject(check.Caution, decision(newDisk), host, true, now)
	if !strings.HasPrefix(s, "[ACTION]") || !strings.Contains(s, "CAUTION") || !strings.Contains(s, "Disk /var at 91%") {
		t.Fatal(s)
	}
}

func TestHeartbeatSubjectSaysAllClear(t *testing.T) {
	s := Subject(check.OK, decision(nil), host, false, now)
	if !strings.HasPrefix(s, "[daily]") || !strings.Contains(s, "all clear") {
		t.Fatal(s)
	}
}

func TestHeartbeatSubjectDisclaimsOngoingFindings(t *testing.T) {
	s := Subject(check.Caution, decision(func(d *alerts.Decision) { d.Ongoing = []check.Alert{disk} }), host, false, now)
	if !strings.HasPrefix(s, "[daily]") || !strings.Contains(s, "no new issues") || !strings.Contains(s, "1 known, unchanged") {
		t.Fatal(s)
	}
}

func TestHeartbeatSubjectStillReportsFindingsItCouldNotEscalate(t *testing.T) {
	s := Subject(check.Caution, decision(newDisk), host, false, now)
	if strings.Contains(s, "all clear") || !strings.Contains(s, "1 new") {
		t.Fatal(s)
	}
}

func TestSubjectDistinguishesNewFromWorsenedFromReminders(t *testing.T) {
	s := Subject(check.Unhealthy, decision(func(d *alerts.Decision) {
		d.New = []check.Alert{disk}
		d.Escalated = []check.Alert{{Status: check.Unhealthy, Msg: "RAM at 99%"}}
		d.Reminders = []check.Alert{{Status: check.Caution, Msg: "8 zombies"}}
	}), host, false, now)
	for _, w := range []string{"1 new", "1 worse", "1 still open"} {
		if !strings.Contains(s, w) {
			t.Errorf("%q missing from %q", w, s)
		}
	}
}

func TestSubjectAlwaysIdentifiesTheHostAndDate(t *testing.T) {
	for _, isAlert := range []bool{true, false} {
		s := Subject(check.Caution, decision(newDisk), host, isAlert, now)
		if !strings.Contains(s, host) || !strings.Contains(s, "2026-01-05") {
			t.Fatal(s)
		}
	}
}
