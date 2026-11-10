package hub

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/report"
)

// certDueListed is how many due nodes a group's line names.
const certDueListed = 5

// certGroups is the digest's "Certificate restarts" block: one entry per
// cert_restart group, from the hosts' own reports (their last restart and
// expiry) and this collector's grants (a granted host that never reported
// again). Nil when no host has cert_restart.before.
func certGroups(entries []Entry, grants []Grant, now time.Time) []report.CertGroup {
	members := map[string][]Entry{}
	for _, e := range entries {
		if e.Err == nil && e.CertRestart != nil && e.CertRestart.Group != "" {
			members[e.CertRestart.Group] = append(members[e.CertRestart.Group], e)
		}
	}
	if len(members) == 0 {
		return nil
	}
	received := map[string]time.Time{}
	for _, e := range entries {
		received[e.Host] = e.ReceivedAt
	}
	clock := func(t time.Time) string { return t.In(now.Location()).Format("15:04") }
	stamp := func(t time.Time) string { return t.In(now.Location()).Format("2006-01-02 15:04") }
	daysTo := func(t time.Time) int { return max(0, int(t.Sub(now)/(24*time.Hour))) }

	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]report.CertGroup, 0, len(names))
	for _, name := range names {
		hosts := members[name]
		g := report.CertGroup{Name: name, Hosts: len(hosts), BeforeDays: hosts[0].CertRestart.BeforeDays}

		// What the hosts say about their own last restart, in the last day.
		for _, e := range hosts {
			c := e.CertRestart
			if c.LastRestart.IsZero() || now.Sub(c.LastRestart) > 24*time.Hour {
				continue
			}
			svc := ""
			if c.Service != "" {
				svc = " " + c.Service
			}
			if c.LastRenewed {
				g.Lines = append(g.Lines, report.CertLine{Status: check.OK, Text: fmt.Sprintf("%s restarted%s at %s, renewed (first expires in %dd)", e.Host, svc, clock(c.LastRestart), daysTo(c.FirstExpiry))})
			} else {
				g.Lines = append(g.Lines, report.CertLine{Status: check.Caution, Text: fmt.Sprintf("%s restarted%s at %s, restart did not renew (first still expires in %dd)", e.Host, svc, clock(c.LastRestart), daysTo(c.FirstExpiry))})
			}
		}

		// A host granted here that has not reported since: it may not have
		// come back.
		latest := map[string]Grant{}
		for _, gr := range grants {
			if gr.Kind == KindCertRestart && gr.Group == name && now.Sub(gr.At) <= grantKeep && gr.At.After(latest[gr.Host].At) {
				latest[gr.Host] = gr
			}
		}
		granted := make([]string, 0, len(latest))
		for h := range latest {
			granted = append(granted, h)
		}
		slices.Sort(granted)
		for _, h := range granted {
			gr := latest[h]
			if !received[h].After(gr.At) {
				g.Lines = append(g.Lines, report.CertLine{Status: check.Unhealthy, Text: fmt.Sprintf("%s was granted a restart at %s, no report since", h, stamp(gr.At))})
			}
		}

		// Nodes due, soonest first.
		var due []Entry
		for _, e := range hosts {
			c := e.CertRestart
			if !c.FirstExpiry.IsZero() && c.FirstExpiry.Sub(now) <= time.Duration(c.BeforeDays)*24*time.Hour {
				due = append(due, e)
			}
		}
		slices.SortStableFunc(due, func(a, b Entry) int { return a.CertRestart.FirstExpiry.Compare(b.CertRestart.FirstExpiry) })
		if len(due) > 0 {
			parts := make([]string, 0, certDueListed)
			for _, e := range due[:min(len(due), certDueListed)] {
				parts = append(parts, fmt.Sprintf("%s (expires in %dd)", e.Host, daysTo(e.CertRestart.FirstExpiry)))
			}
			text := "due: " + strings.Join(parts, ", ")
			if len(due) > certDueListed {
				text += fmt.Sprintf(" (+%d more)", len(due)-certDueListed)
			}
			g.Lines = append(g.Lines, report.CertLine{Status: check.Info, Text: text})
		}
		out = append(out, g)
	}
	return out
}
