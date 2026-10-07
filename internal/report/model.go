// Package report turns one run into what people read: an HTML e-mail, a
// fixed-width text report, and JSON for tools. All three render the same
// Model, so they cannot disagree about ordering or triage.
package report

import (
	"slices"
	"time"

	"github.com/thyarles/lhc-go/internal/alerts"
	"github.com/thyarles/lhc-go/internal/check"
)

// Triage is the "why am I reading this" part: what is new, what is known.
type Triage struct {
	NotifyAll bool     `json:"notify_all"`
	Summary   string   `json:"summary"`
	New       []string `json:"new"`
	Escalated []string `json:"escalated"`
	Reminders []string `json:"reminders"`
	Ongoing   []string `json:"ongoing"`
	Resolved  []string `json:"resolved"`
}

// Model is one run, ready to render.
type Model struct {
	Host      string
	Version   string
	Generated time.Time
	Overall   check.Status
	// Sections are worst first; within a severity band the check order is
	// kept. A reader should not scroll past nine green panels to reach the
	// one that needs them.
	Sections []*check.Section
	Triage   *Triage // nil when there was no alert evaluation
}

// Build assembles the model. d may be nil.
func Build(res check.Result, d *alerts.Decision, host, version string, now time.Time) *Model {
	m := &Model{
		Host: host, Version: version, Generated: now, Overall: res.Overall,
		Sections: Order(res.Sections),
	}
	if d != nil {
		m.Triage = &Triage{
			NotifyAll: d.NotifyAll,
			Summary:   d.Summary(),
			New:       msgs(d.New),
			Escalated: msgs(d.Escalated),
			Reminders: msgs(d.Reminders),
			Ongoing:   msgs(d.Ongoing),
			Resolved:  append([]string{}, d.Resolved...),
		}
	}
	return m
}

func msgs(as []check.Alert) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Msg)
	}
	return out
}

// Order sorts worst first, stable within a severity band.
func Order(in []*check.Section) []*check.Section {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b *check.Section) int { return int(b.Status) - int(a.Status) })
	return out
}

// Applicable are the sections that get a panel of their own.
func (m *Model) Applicable() []*check.Section {
	var out []*check.Section
	for _, s := range m.Sections {
		if s.Applicable {
			out = append(out, s)
		}
	}
	return out
}

// Skipped are the titles of the sections with nothing to inspect here.
func (m *Model) Skipped() []string {
	var out []string
	for _, s := range m.Sections {
		if !s.Applicable {
			out = append(out, s.Title)
		}
	}
	return out
}

// VersionLabel is "v1.2.3", or "dev" for an unreleased build.
func (m *Model) VersionLabel() string {
	if m.Version == "" || m.Version == "dev" {
		return "dev"
	}
	return "v" + m.Version
}

// Stamp is the timestamp with its zone, so reports from several hosts can be
// compared.
func (m *Model) Stamp() string { return m.Generated.Format("2006-01-02 15:04 MST") }

// triageGroup is one titled list inside the triage block.
type triageGroup struct {
	Title  string
	Items  []string
	Status check.Status
}

func (t *Triage) groups() []triageGroup {
	all := []triageGroup{
		{"Needs attention — new", t.New, check.Unhealthy},
		{"Got worse", t.Escalated, check.Unhealthy},
		{"Still open — reminder", t.Reminders, check.Caution},
		{"Known, unchanged — no action implied", t.Ongoing, check.Info},
		{"Cleared since last run", t.Resolved, check.OK},
	}
	var out []triageGroup
	for _, g := range all {
		if len(g.Items) > 0 {
			out = append(out, g)
		}
	}
	return out
}
