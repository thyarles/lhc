// Package alerts decides whether a finding interrupts anyone.
//
// The report is a snapshot; a notification is an interruption. A disk that
// has been at 91% for three weeks is a true CAUTION in every snapshot, but it
// is only worth interrupting a broad audience about once — otherwise the
// audience learns that CAUTION means "ignore me".
//
// This package keeps the conditions that have already been notified, so the
// notifier can answer a different question from "what is the status?":
// "is there anything here these people have not already been told?"
package alerts

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/state"
)

// StateKey is where the notified conditions live in the "alerts" store.
const StateKey = "conditions"

// Numbers inside an alert message change constantly ("83 pending updates"
// -> "84 pending updates") and must not make an ongoing condition look new.
var digits = regexp.MustCompile(`\d+`)

// Fingerprint is the stable identity of a condition, ignoring numbers.
func Fingerprint(msg string) string {
	return strings.ToLower(strings.TrimSpace(digits.ReplaceAllString(msg, "#")))
}

// Entry is one remembered condition.
type Entry struct {
	Fingerprint      string       `json:"fingerprint"`
	Status           check.Status `json:"status"`
	Msg              string       `json:"msg"`
	FirstSeen        time.Time    `json:"first_seen"`
	LastSeen         time.Time    `json:"last_seen"`
	NotifiedAt       time.Time    `json:"notified_at"`
	NotifiedStatus   check.Status `json:"notified_status"`
	ResolvedReported bool         `json:"resolved_reported,omitempty"`
}

// Policy is the alerts: block of the config.
type Policy struct {
	// Minimum severity that reaches the broad list. Anything other than
	// Unhealthy means Caution.
	NotifyAllOn     check.Status
	RemindCaution   time.Duration // 0 = never remind
	RemindUnhealthy time.Duration
	ForgetAfter     time.Duration
}

// DefaultPolicy matches the defaults in the config.
var DefaultPolicy = Policy{
	NotifyAllOn:     check.Unhealthy,
	RemindCaution:   168 * time.Hour,
	RemindUnhealthy: 24 * time.Hour,
	ForgetAfter:     72 * time.Hour,
}

// Decision is what to send, to whom, and why.
type Decision struct {
	New       []check.Alert // never notified before
	Escalated []check.Alert // known, but worse than when last notified
	Ongoing   []check.Alert // known and unchanged
	Reminders []check.Alert // ongoing past its reminder age
	Resolved  []string      // messages that cleared since the last run

	NotifyAll bool
	Reason    string

	pending []Entry
	store   state.Store
}

// Actionable is everything a human should act on in this run.
func (d *Decision) Actionable() []check.Alert {
	out := make([]check.Alert, 0, len(d.New)+len(d.Escalated)+len(d.Reminders))
	out = append(out, d.New...)
	out = append(out, d.Escalated...)
	return append(out, d.Reminders...)
}

// Summary is the one-line triage, e.g. "1 new, 2 still open".
func (d *Decision) Summary() string {
	var bits []string
	if n := len(d.New); n > 0 {
		bits = append(bits, fmt.Sprintf("%d new", n))
	}
	if n := len(d.Escalated); n > 0 {
		bits = append(bits, fmt.Sprintf("%d worsened", n))
	}
	if n := len(d.Reminders); n > 0 {
		bits = append(bits, fmt.Sprintf("%d still open", n))
	}
	if n := len(d.Ongoing); n > 0 && len(bits) == 0 {
		bits = append(bits, fmt.Sprintf("%d ongoing", n))
	}
	if n := len(d.Resolved); n > 0 {
		bits = append(bits, fmt.Sprintf("%d resolved", n))
	}
	if len(bits) == 0 {
		return "nothing to report"
	}
	return strings.Join(bits, ", ")
}

// Commit records this run's conditions as notified.
//
// Deliberately separate from Evaluate: call it only once the message has
// been accepted by the relay (or there was nobody to send it to). Otherwise a
// failed send would mark a brand new alert as delivered and nobody would ever
// hear about it.
func (d *Decision) Commit() error {
	if d.store == nil {
		return nil
	}
	return d.store.Save(StateKey, d.pending)
}

// age since t; a zero time is "forever ago".
func age(t, now time.Time) time.Duration {
	if t.IsZero() {
		return 1 << 62
	}
	return now.Sub(t)
}

// Evaluate compares this run's alerts with what has already been notified.
// It reads the store but never writes it; see Commit.
func Evaluate(alerts []check.Alert, store state.Store, p Policy, now time.Time) *Decision {
	if p.NotifyAllOn != check.Unhealthy {
		p.NotifyAllOn = check.Caution
	}
	d := &Decision{store: store}

	var prev []Entry
	if store != nil && !store.Load(StateKey, &prev) {
		prev = nil
	}
	prevByFP := make(map[string]Entry, len(prev))
	for _, e := range prev {
		prevByFP[e.Fingerprint] = e
	}

	seenIdx := map[string]int{}
	var seen []Entry
	for _, a := range alerts {
		fp := Fingerprint(a.Msg)
		// Collapse duplicates within one run (same condition, new number).
		if i, ok := seenIdx[fp]; ok {
			seen[i].Status = check.Worse(seen[i].Status, a.Status)
			continue
		}
		old, known := prevByFP[fp]
		// Clear for longer than the forget window: a genuine recurrence,
		// not a continuation. Notify about it again.
		if known && age(old.LastSeen, now) >= p.ForgetAfter {
			known = false
		}

		var e Entry
		if !known {
			e = Entry{
				Fingerprint: fp, Status: a.Status, Msg: a.Msg,
				FirstSeen: now, LastSeen: now, NotifiedAt: now, NotifiedStatus: a.Status,
			}
			d.New = append(d.New, a)
		} else {
			e = old
			e.Status, e.Msg, e.LastSeen = a.Status, a.Msg, now
			// Present again: if it clears later, that is news again.
			e.ResolvedReported = false
			remind := p.RemindCaution
			if a.Status == check.Unhealthy {
				remind = p.RemindUnhealthy
			}
			switch {
			case a.Status > e.NotifiedStatus:
				d.Escalated = append(d.Escalated, a)
				e.NotifiedAt, e.NotifiedStatus = now, a.Status
			case remind > 0 && age(e.NotifiedAt, now) >= remind:
				d.Reminders = append(d.Reminders, a)
				e.NotifiedAt, e.NotifiedStatus = now, a.Status
			default:
				d.Ongoing = append(d.Ongoing, a)
			}
		}
		seenIdx[fp] = len(seen)
		seen = append(seen, e)
	}

	// Conditions that were present before and are gone now.
	for _, old := range prev {
		if _, ok := seenIdx[old.Fingerprint]; ok {
			continue
		}
		if age(old.LastSeen, now) < p.ForgetAfter {
			// Keep it briefly so a flapping condition does not re-page on
			// every cycle, but mention that it cleared exactly once.
			e := old
			if !e.ResolvedReported {
				msg := e.Msg
				if msg == "" {
					msg = e.Fingerprint
				}
				d.Resolved = append(d.Resolved, msg)
				e.ResolvedReported = true
			}
			seenIdx[e.Fingerprint] = len(seen)
			seen = append(seen, e)
		}
	}
	d.pending = seen

	// Does the broad list hear about this run?
	worth := 0
	for _, a := range d.Actionable() {
		if a.Status >= p.NotifyAllOn {
			worth++
		}
	}
	switch {
	case worth > 0:
		d.NotifyAll = true
		var parts []string
		if n := len(d.New); n > 0 {
			parts = append(parts, fmt.Sprintf("%d new condition(s)", n))
		}
		if n := len(d.Escalated); n > 0 {
			parts = append(parts, fmt.Sprintf("%d worsened", n))
		}
		if n := len(d.Reminders); n > 0 {
			parts = append(parts, fmt.Sprintf("%d still unresolved", n))
		}
		d.Reason = strings.Join(parts, "; ")
	case len(d.Ongoing) > 0:
		d.Reason = fmt.Sprintf("%d known condition(s) unchanged since the last notification — report only, no alert sent", len(d.Ongoing))
	default:
		d.Reason = "no conditions requiring attention"
	}
	return d
}
