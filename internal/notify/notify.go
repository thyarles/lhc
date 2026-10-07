// Package notify decides who hears about a run and delivers it. SMTP is the
// only transport today; anything else implements Notifier.
package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/alerts"
	"github.com/thyarles/lhc-go/internal/check"
)

// Message is one delivery.
type Message struct {
	From    string
	To      []string
	Subject string
	Text    string
	HTML    string
	// HTMLMode: inline (HTML body with a text alternative), attachment (text
	// body, HTML as a file) or both.
	HTMLMode string
}

// Notifier delivers a message. A nil error means the transport accepted it;
// only then may the alert history be committed.
type Notifier interface {
	Send(ctx context.Context, m Message) error
}

// PlanDelivery decides who receives this run's message, and whether it is
// an alert.
//
// Two audiences ask two different questions:
//
//	daily — "is the check still running?"      → always hears something
//	broad — "is there something for me to do?" → only when there is
//
// An alert covers both lists in ONE message, so nobody gets the same report
// twice. With no broad list configured the run degrades to a heartbeat rather
// than dropping the findings on the floor.
func PlanDelivery(d *alerts.Decision, daily, broad []string) (to []string, isAlert bool) {
	if d.NotifyAll && len(broad) > 0 {
		to = slices.Clone(broad)
		for _, r := range daily {
			if !slices.Contains(to, r) {
				to = append(to, r)
			}
		}
		return to, true
	}
	return slices.Clone(daily), false
}

// subjectLabel is the status as the subject line shows it. Mail clients
// render these glyphs in the inbox list, where the word alone is easy to skim
// past.
var subjectLabel = map[check.Status]string{
	check.OK: "✓ OK", check.Info: "· INFO", check.Caution: "⚠ CAUTION", check.Unhealthy: "✖ UNHEALTHY",
}

// Subject lines mean different things to the two audiences. Only a message
// with something new to act on carries [ACTION]; the heartbeat says "no new
// issues" outright, so a reader can tell from the subject alone whether it
// needs them.
func Subject(overall check.Status, d *alerts.Decision, host string, isAlert bool, now time.Time) string {
	date := now.Format("2006-01-02")
	label := subjectLabel[overall]
	act := d.Actionable()
	if isAlert {
		var top []string
		for _, a := range act[:min(2, len(act))] {
			top = append(top, a.Msg)
		}
		return fmt.Sprintf("[ACTION] %s · %s · %s — %s", label, host, date, strings.Join(top, " | "))
	}
	// The heartbeat still describes what is in the report. It carries
	// findings when the broad list is unset, or when notify_all_on is
	// "unhealthy" and the findings are only CAUTION.
	if len(act) > 0 {
		var bits []string
		if n := len(d.New); n > 0 {
			bits = append(bits, fmt.Sprintf("%d new", n))
		}
		if n := len(d.Escalated); n > 0 {
			bits = append(bits, fmt.Sprintf("%d worse", n))
		}
		if n := len(d.Reminders); n > 0 {
			bits = append(bits, fmt.Sprintf("%d still open", n))
		}
		return fmt.Sprintf("[daily] %s · %s · %s — %s", label, host, date, strings.Join(bits, ", "))
	}
	if n := len(d.Ongoing); n > 0 {
		return fmt.Sprintf("[daily] %s (no new issues) · %s · %s — %d known, unchanged", label, host, date, n)
	}
	return fmt.Sprintf("[daily] %s · %s · %s — all clear", label, host, date)
}
