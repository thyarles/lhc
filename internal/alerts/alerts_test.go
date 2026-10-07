package alerts

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/state"
)

// The de-duplication state machine decides whether a finding interrupts the
// broad audience or just appears in the report. It carries the regression the
// whole design is about: the same unfixed problem must not notify everyone
// every day.

var day0 = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)

func at(days, hours float64) time.Time {
	return day0.Add(time.Duration((days*24 + hours) * float64(time.Hour)))
}

// cautionPolicy pins the threshold to CAUTION: every finding in this file is
// CAUTION-level, and leaving the threshold to the shipped default would make
// these tests depend on a product decision they are not about.
func cautionPolicy() Policy {
	p := DefaultPolicy
	p.NotifyAllOn = check.Caution
	return p
}

var disk = check.Alert{Status: check.Caution, Msg: "Disk /var at 91%"}

func run(t *testing.T, s state.Store, p Policy, alerts []check.Alert, when time.Time) *Decision {
	t.Helper()
	d := Evaluate(alerts, s, p, when)
	if err := d.Commit(); err != nil {
		t.Fatal(err)
	}
	return d
}

func msgs(as []check.Alert) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.Msg)
	}
	return out
}

func notifiedOn(t *testing.T, p Policy, alerts []check.Alert, days int) []int {
	s := state.NewMemory()
	var out []int
	for day := 1; day <= days; day++ {
		if run(t, s, p, alerts, at(float64(day), 0)).NotifyAll {
			out = append(out, day)
		}
	}
	return out
}

// ── fingerprinting ──────────────────────────────────────────────────────────

func TestFingerprintIgnoresDriftingNumbers(t *testing.T) {
	if Fingerprint("83 pending updates") != Fingerprint("84 pending updates") ||
		Fingerprint("Disk /var at 91%") != Fingerprint("Disk /var at 92%") {
		t.Fatal("numbers changed the fingerprint")
	}
}

func TestFingerprintSeparatesGenuinelyDifferentConditions(t *testing.T) {
	if Fingerprint("Disk /var at 91%") == Fingerprint("Disk /home at 91%") {
		t.Fatal("different mounts collapsed")
	}
}

func TestFingerprintIsCaseInsensitiveAndTrimmed(t *testing.T) {
	if Fingerprint("  Disk Full  ") != Fingerprint("disk full") {
		t.Fatal("case/space changed the fingerprint")
	}
}

// ── the core regression ─────────────────────────────────────────────────────

func TestUnchangedProblemNotifiesOnceNotEveryDay(t *testing.T) {
	if got := notifiedOn(t, cautionPolicy(), []check.Alert{disk}, 7); !slices.Equal(got, []int{1}) {
		t.Fatalf("an unfixed problem must interrupt the broad list once, not daily: %v", got)
	}
}

func TestStillOpenProblemGetsAWeeklyReminder(t *testing.T) {
	if got := notifiedOn(t, cautionPolicy(), []check.Alert{disk}, 21); !slices.Equal(got, []int{1, 8, 15}) {
		t.Fatalf("got %v", got)
	}
}

func TestReminderCanBeDisabled(t *testing.T) {
	p := cautionPolicy()
	p.RemindCaution = 0
	if got := notifiedOn(t, p, []check.Alert{disk}, 21); !slices.Equal(got, []int{1}) {
		t.Fatalf("got %v", got)
	}
}

func TestUnhealthyIsChasedDaily(t *testing.T) {
	bad := []check.Alert{{Status: check.Unhealthy, Msg: "Disk /var at 99%"}}
	if got := notifiedOn(t, cautionPolicy(), bad, 5); !slices.Equal(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("got %v", got)
	}
}

// ── transitions ─────────────────────────────────────────────────────────────

func TestNewConditionNotifies(t *testing.T) {
	d := run(t, state.NewMemory(), cautionPolicy(), []check.Alert{disk}, at(1, 0))
	if !d.NotifyAll || !slices.Equal(msgs(d.New), []string{"Disk /var at 91%"}) || len(d.Ongoing) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestSecondRunReportsOngoingWithoutNotifying(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	d := run(t, s, cautionPolicy(), []check.Alert{disk}, at(2, 0))
	if d.NotifyAll || !slices.Equal(msgs(d.Ongoing), []string{"Disk /var at 91%"}) || len(d.New) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestEscalationFromCautionToUnhealthyNotifiesImmediately(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	d := run(t, s, cautionPolicy(), []check.Alert{{Status: check.Unhealthy, Msg: "Disk /var at 97%"}}, at(2, 0))
	if !d.NotifyAll || !slices.Equal(msgs(d.Escalated), []string{"Disk /var at 97%"}) {
		t.Fatalf("%+v", d)
	}
}

func TestDeEscalationDoesNotNotify(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{{Status: check.Unhealthy, Msg: "Disk /var at 97%"}}, at(1, 0))
	d := run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 2))
	if d.NotifyAll || !slices.Equal(msgs(d.Ongoing), []string{"Disk /var at 91%"}) {
		t.Fatalf("%+v", d)
	}
}

func TestAGenuinelyNewConditionNotifiesEvenWhileAnotherIsOngoing(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	d := run(t, s, cautionPolicy(), []check.Alert{disk, {Status: check.Caution, Msg: "3 root login(s) today"}}, at(2, 0))
	if !d.NotifyAll || !slices.Equal(msgs(d.New), []string{"3 root login(s) today"}) ||
		!slices.Equal(msgs(d.Ongoing), []string{"Disk /var at 91%"}) {
		t.Fatalf("%+v", d)
	}
}

func TestClearedConditionIsReportedOnceThenStaysQuiet(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	cleared := run(t, s, cautionPolicy(), nil, at(2, 0))
	if !slices.Equal(cleared.Resolved, []string{"Disk /var at 91%"}) || cleared.NotifyAll {
		t.Fatalf("%+v", cleared)
	}
	if quiet := run(t, s, cautionPolicy(), nil, at(3, 0)); len(quiet.Resolved) != 0 {
		t.Fatal("a resolved condition must not be re-announced daily")
	}
}

func TestAConditionThatComesBackAndClearsAgainIsReportedAgain(t *testing.T) {
	// The Python version kept resolved_reported across a recurrence, so the
	// second clearing went unmentioned.
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	run(t, s, cautionPolicy(), nil, at(1, 6))
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 12))
	if d := run(t, s, cautionPolicy(), nil, at(1, 18)); !slices.Equal(d.Resolved, []string{"Disk /var at 91%"}) {
		t.Fatalf("second clearing not reported: %+v", d)
	}
}

func TestRecurrenceAfterTheForgetWindowCountsAsNew(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	run(t, s, cautionPolicy(), nil, at(2, 0)) // clears
	run(t, s, cautionPolicy(), nil, at(3, 0)) // still clear
	run(t, s, cautionPolicy(), nil, at(4, 0)) // forgotten (> 72h)
	d := run(t, s, cautionPolicy(), []check.Alert{disk}, at(10, 0))
	if !d.NotifyAll || !slices.Equal(msgs(d.New), []string{"Disk /var at 91%"}) {
		t.Fatalf("%+v", d)
	}
}

func TestDriftingNumberDoesNotLookLikeANewCondition(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{{Status: check.Caution, Msg: "83 pending updates"}}, at(1, 0))
	d := run(t, s, cautionPolicy(), []check.Alert{{Status: check.Caution, Msg: "84 pending updates"}}, at(2, 0))
	if d.NotifyAll || len(d.New) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestDuplicateConditionsWithinOneRunAreCollapsed(t *testing.T) {
	d := run(t, state.NewMemory(), cautionPolicy(), []check.Alert{disk, {Status: check.Caution, Msg: "Disk /var at 92%"}}, at(1, 0))
	if len(d.New) != 1 {
		t.Fatalf("%+v", d.New)
	}
}

// ── notify_all_on threshold ─────────────────────────────────────────────────

func TestThresholdUnhealthySuppressesCautionAlerts(t *testing.T) {
	if run(t, state.NewMemory(), DefaultPolicy, []check.Alert{disk}, at(1, 0)).NotifyAll {
		t.Fatal("caution reached the broad list under notify_all_on=unhealthy")
	}
}

func TestThresholdUnhealthyStillAlertsOnUnhealthy(t *testing.T) {
	d := run(t, state.NewMemory(), DefaultPolicy, []check.Alert{{Status: check.Unhealthy, Msg: "RAM at 99%"}}, at(1, 0))
	if !d.NotifyAll {
		t.Fatal("unhealthy did not notify")
	}
}

func TestInvalidThresholdFallsBackToCaution(t *testing.T) {
	p := DefaultPolicy
	p.NotifyAllOn = check.Info
	if !run(t, state.NewMemory(), p, []check.Alert{disk}, at(1, 0)).NotifyAll {
		t.Fatal("invalid threshold did not fall back to caution")
	}
}

// ── delivery failure ────────────────────────────────────────────────────────

func TestUncommittedAlertIsRetriedNextRun(t *testing.T) {
	s := state.NewMemory()
	first := Evaluate([]check.Alert{disk}, s, cautionPolicy(), at(1, 0))
	if !first.NotifyAll {
		t.Fatal("first run did not notify")
	}
	// send fails → no commit
	second := Evaluate([]check.Alert{disk}, s, cautionPolicy(), at(2, 0))
	if !second.NotifyAll || !slices.Equal(msgs(second.New), []string{"Disk /var at 91%"}) {
		t.Fatal("alert was swallowed by a failed delivery")
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	if Evaluate([]check.Alert{disk}, s, cautionPolicy(), at(3, 0)).NotifyAll {
		t.Fatal("committed alert notified again")
	}
}

func TestEvaluateDoesNotWriteStateUntilCommit(t *testing.T) {
	s := state.NewMemory()
	Evaluate([]check.Alert{disk}, s, cautionPolicy(), at(1, 0))
	if s.Has(StateKey) {
		t.Fatal("Evaluate wrote state")
	}
}

func TestCorruptStateReadsAsNoHistory(t *testing.T) {
	s := state.NewMemory()
	if err := s.Save(StateKey, map[string]string{"not": "a list"}); err != nil {
		t.Fatal(err)
	}
	if d := Evaluate([]check.Alert{disk}, s, cautionPolicy(), at(1, 0)); len(d.New) != 1 {
		t.Fatalf("%+v", d)
	}
}

// ── reporting helpers ───────────────────────────────────────────────────────

func TestSummaryDescribesAQuietRun(t *testing.T) {
	if got := run(t, state.NewMemory(), cautionPolicy(), nil, at(1, 0)).Summary(); got != "nothing to report" {
		t.Fatalf("got %q", got)
	}
}

func TestReasonExplainsWhyNoAlertWasSent(t *testing.T) {
	s := state.NewMemory()
	run(t, s, cautionPolicy(), []check.Alert{disk}, at(1, 0))
	d := run(t, s, cautionPolicy(), []check.Alert{disk}, at(2, 0))
	if !strings.Contains(d.Reason, "unchanged") || !strings.Contains(d.Reason, "no alert sent") {
		t.Fatalf("reason %q", d.Reason)
	}
}
