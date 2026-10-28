package report

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	tw "github.com/thyarles/lhc/internal/textwrap"
)

// Digest is a collector's daily summary of every host's latest report.
type Digest struct {
	Collector   string // the collector sending it
	Version     string
	Generated   time.Time
	SilentAfter time.Duration
	Overall     check.Status
	// Collectors are this one first, then the configured peers.
	Collectors []DigestPeer
	Silent     []DigestHost // no report for longer than SilentAfter
	NotOK      []DigestHost // CAUTION or worse, worst first
	NewHosts   []string     // first seen since the previous digest
	Total, OK  int
	// Sums over the hosts' vital signs; a vital no host reported is zero.
	UptimeMin, UptimeMax time.Duration
	Updates              int
	Reboot               int // hosts waiting for a reboot
	// The newest lhc any host runs and how many run it; "" when every host
	// runs the same version. Older is "host (version)" for the hosts behind
	// it, at most OlderListed, by name; OlderMore counts the rest.
	Newest      string
	NewestHosts int
	Older       []string
	OlderMore   int
}

// OlderListed is how many hosts behind the newest version the digest names.
const OlderListed = 5

// DigestHost is one host the digest names.
type DigestHost struct {
	Host   string
	Status check.Status
	Last   time.Time // the collector's latest report from it
	New    bool      // OK in the previous digest
	Alerts []string  // its first two alerts, worst first
	More   int       // alerts beyond those
}

// DigestPeer is a collector of the same hub. It is alive when its own
// host report keeps arriving here.
type DigestPeer struct {
	Host   string
	Self   bool
	Last   time.Time // zero: never reported here
	Silent bool
	// Set on this collector when its own host report, made by the installed
	// binary, is newer than the running `lhc collect`: an upgrade that did
	// not restart the service (#36).
	Running, Installed string
}

// Count is how many hosts are at a status, the silent ones apart.
func (d *Digest) Count(st check.Status) int {
	n := 0
	for _, h := range d.NotOK {
		if h.Status == st {
			n++
		}
	}
	return n
}

// Tally is the subject's counts: "20 hosts · 9 OK · 6 CAUTION · 1 SILENT".
// A status no host is at is left out, except OK.
func (d *Digest) Tally() string {
	parts := []string{plural(d.Total, "host"), fmt.Sprintf("%d OK", d.OK)}
	for _, st := range []check.Status{check.Caution, check.Unhealthy} {
		if n := d.Count(st); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st.Word()))
		}
	}
	if n := len(d.Silent); n > 0 {
		parts = append(parts, fmt.Sprintf("%d SILENT", n))
	}
	return strings.Join(parts, " · ")
}

// DigestSubject tags the digest [hub], apart from [daily] and [ACTION].
func DigestSubject(d *Digest) string {
	s := "[hub] " + d.Tally() + " · via " + d.Collector
	for _, p := range d.Collectors {
		if p.Silent {
			s += " · peer " + p.Host + " SILENT"
		}
	}
	return s
}

// Stamp is the time the digest was built, with its zone.
func (d *Digest) Stamp() string { return d.Generated.Format("2006-01-02 15:04 MST") }

// VersionLabel is "v1.2.3", or "dev" for an unreleased build.
func (d *Digest) VersionLabel() string { return (&Model{Version: d.Version}).VersionLabel() }

// Since is a report time as the digest prints it: "2026-10-21 03:12 (5h ago)".
func (d *Digest) Since(t time.Time) string {
	if t.IsZero() {
		return "never reported here"
	}
	return t.In(d.Generated.Location()).Format("2006-01-02 15:04") + " (" + Ago(d.Generated.Sub(t)) + " ago)"
}

// Status is a collector's state in the digest.
func (p DigestPeer) Status() check.Status {
	switch {
	case p.Silent:
		return check.Unhealthy
	case p.Installed != "":
		return check.Caution
	}
	return check.OK
}

// PeerText is what the digest says about a collector.
func (d *Digest) PeerText(p DigestPeer) string {
	switch {
	case p.Silent && p.Last.IsZero():
		return "SILENT, never reported here"
	case p.Silent:
		return "SILENT since " + d.Since(p.Last)
	case p.Self && p.Installed != "":
		return "this one · running lhc " + p.Running + ", installed " + p.Installed + ": restart lhc-collect"
	case p.Self:
		return "this one"
	}
	return "last report " + d.Since(p.Last)
}

// Totals is the "everyone else" line of vital-sign sums, or "" when no
// host reported any.
func (d *Digest) Totals() string {
	var parts []string
	if d.UptimeMax > 0 {
		parts = append(parts, "uptime "+span(d.UptimeMin)+" to "+span(d.UptimeMax))
	}
	if d.Updates > 0 {
		parts = append(parts, plural(d.Updates, "pending update"))
	}
	if d.Reboot > 0 {
		parts = append(parts, plural(d.Reboot, "host")+" waiting for a reboot")
	}
	return strings.Join(parts, " · ")
}

// VersionsLine names the hosts behind the newest lhc, or "" when every
// host runs the same version.
func (d *Digest) VersionsLine() string {
	if d.Newest == "" {
		return ""
	}
	s := fmt.Sprintf("lhc %s on %s · older: %s", d.Newest, plural(d.NewestHosts, "host"), strings.Join(d.Older, ", "))
	if d.OlderMore > 0 {
		s += fmt.Sprintf(" (+%d more)", d.OlderMore)
	}
	return s
}

// Ago reads like a person would say it: 45m, 3h, 2d 4h.
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())-days*24)
	}
}

// Hours is a setting such as silent_after: "26h", or "2 days" when whole.
func Hours(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return plural(int(d/(24*time.Hour)), "day")
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// span is an uptime the way the hosts' reports write it.
func span(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d >= time.Hour:
		return plural(int(d/time.Hour), "hour")
	default:
		return plural(int(d/time.Minute), "minute")
	}
}

func plural(n int, unit string) string {
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// hostW is the host column. A longer name gets a line of its own rather
// than being cut: the name is what the reader acts on.
const hostW = 30

// DigestText renders the digest in W columns: the mail's plain-text part
// and what `lhc hub digest --dry-run` prints.
func DigestText(d *Digest) string {
	ov := Pal(d.Overall)
	lines := []string{
		strings.Repeat("=", W),
		tw.FitHost("  lhc hub digest "+d.VersionLabel()+"  ·  via {host}", d.Collector, W),
		"  " + d.Stamp(),
		fmt.Sprintf("  Overall Status: %s  %s", ov.Sym, ov.Word),
		"  " + d.Tally(),
		strings.Repeat("=", W),
	}

	lines = append(lines, "", "  COLLECTORS")
	for _, p := range d.Collectors {
		lines = append(lines, hostLines(p.Host, p.Status(), d.PeerText(p))...)
		if p.Silent {
			lines = append(lines, detailLines("this may be the only digest today")...)
		}
	}

	if len(d.Silent) > 0 {
		lines = append(lines, "", "  SILENT — no report for over "+Hours(d.SilentAfter))
		for _, h := range d.Silent {
			lines = append(lines, hostLines(h.Host, check.Unhealthy, "last report "+d.Since(h.Last))...)
		}
	}

	if len(d.NotOK) > 0 {
		lines = append(lines, "", "  NOT OK")
		for _, h := range d.NotOK {
			what := h.Status.Word()
			if h.New {
				what += " · new"
			}
			lines = append(lines, hostLines(h.Host, h.Status, what)...)
			for _, a := range h.Alerts {
				lines = append(lines, detailLines("- "+a)...)
			}
			if h.More > 0 {
				lines = append(lines, detailLines(fmt.Sprintf("(+%d more)", h.More))...)
			}
		}
	}

	if len(d.NewHosts) > 0 {
		lines = append(lines, "", "  NEW HOSTS since the last digest")
		for _, h := range d.NewHosts {
			lines = append(lines, "     "+h)
		}
	}

	lines = append(lines, "", "  EVERYONE ELSE", "  "+plural(d.OK, "host")+" OK")
	for _, t := range []string{d.Totals(), d.VersionsLine()} {
		if t == "" {
			continue
		}
		wrapped := tw.Wrap(t, W-2, "  ")
		wrapped[0] = "  " + wrapped[0]
		lines = append(lines, wrapped...)
	}
	lines = append(lines, "",
		"  A host that was shut down for good stays SILENT here until you run",
		"  `lhc hub forget HOST` on each collector.",
		strings.Repeat("=", W))
	return strings.Join(lines, "\n") + "\n"
}

// hostLines is "  ✕  host-name                      what", the name on a
// line of its own when it is longer than the column.
func hostLines(host string, st check.Status, what string) []string {
	prefix := "  " + Pal(st).Sym + "  "
	indent := strings.Repeat(" ", tw.Len(prefix)+hostW+1)
	var out []string
	if tw.Len(host) > hostW {
		out = append(out, prefix+tw.Truncate(host, W-tw.Len(prefix)))
		prefix = indent
	} else {
		prefix += tw.Pad(host, hostW) + " "
	}
	for i, l := range tw.Wrap(what, W-tw.Len(indent), indent) {
		if i == 0 {
			l = prefix + l
		}
		out = append(out, l)
	}
	return out
}

// detailLines is a line under a host, indented past the glyph.
func detailLines(s string) []string {
	const indent = "       "
	out := tw.Wrap(s, W-len(indent)-2, indent+"  ")
	out[0] = indent + out[0]
	return out
}

//go:embed digest.html.tmpl
var digestSource string

// dhost is one host row of the HTML digest.
type dhost struct {
	Host  string
	St    check.Status
	What  string
	Lines []string
}

// The digest reuses the report's palette, theme and status chip.
var digestTmpl = template.Must(template.Must(htmlTmpl.Clone()).Funcs(template.FuncMap{
	"row": func(host string, st check.Status, what string, lines ...string) dhost {
		return dhost{host, st, what, lines}
	},
	"notok": func(h DigestHost) dhost {
		what := h.Status.Word()
		if h.New {
			what += " · new"
		}
		lines := h.Alerts
		if h.More > 0 {
			lines = append(append([]string{}, lines...), fmt.Sprintf("(+%d more)", h.More))
		}
		return dhost{h.Host, h.Status, what, lines}
	},
	"hours":  Hours,
	"plural": plural,
}).New("digest").Parse(digestSource))

// DigestHTML renders the digest's mail body.
func DigestHTML(d *Digest) (string, error) {
	var buf bytes.Buffer
	if err := digestTmpl.ExecuteTemplate(&buf, "digest", d); err != nil {
		return "", err
	}
	return buf.String(), nil
}
