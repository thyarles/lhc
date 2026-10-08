package report

import (
	"fmt"
	"strings"

	"github.com/thyarles/lhc/internal/check"
	tw "github.com/thyarles/lhc/internal/textwrap"
)

// W is the width every line of the text report fits inside.
const W = 78

// labelW is the gutter for the label column.
const labelW = 30

// Text renders the fixed-width report: the terminal view and the plain-text
// part of the e-mail.
func Text(m *Model) string {
	ov := Pal(m.Overall)
	lines := []string{
		strings.Repeat("=", W),
		tw.FitHost("  Linux Health Check "+m.VersionLabel()+"  ·  {host}", m.Host, W),
		"  " + m.Stamp(),
		fmt.Sprintf("  Overall Status: %s  %s", ov.Sym, ov.Word),
	}
	lines = append(lines, vitalsText(m.Vitals)...)
	lines = append(lines, strings.Repeat("=", W))
	lines = append(lines, triageText(m.Triage)...)
	lines = append(lines, summaryText(m)...)
	lines = append(lines, legendText())

	for _, sec := range m.Applicable() {
		p := Pal(sec.Status)
		lines = append(lines, "",
			fmt.Sprintf("  %s  %s  [%s]", p.Sym, sec.Title, p.Word),
			"  "+strings.Repeat("─", W-4))
		for _, r := range sec.Rows {
			if r.Separator {
				lines = append(lines, "     "+r.Label)
				continue
			}
			lines = append(lines, rowLines(r)...)
		}
	}
	lines = append(lines, "", strings.Repeat("=", W))
	return strings.Join(lines, "\n") + "\n"
}

// vitalsText packs the strip into as few lines as fit, breaking only between
// facts. A flagged fact carries its status glyph, so it stands out without
// colour. One fact too long for a line on its own (a deep mount point) is
// cut with "…".
func vitalsText(facts []check.Fact) []string {
	const indent, sep = "  ", " · "
	var lines []string
	cur := ""
	for _, f := range facts {
		item := VitalText(f)
		if f.Status.Flagged() {
			item = Pal(f.Status).Sym + " " + item
		}
		item = tw.Truncate(item, W-len(indent))
		switch {
		case cur == "":
			cur = item
		case tw.Len(indent+cur+sep+item) <= W:
			cur += sep + item
		default:
			lines = append(lines, indent+cur)
			cur = item
		}
	}
	if cur != "" {
		lines = append(lines, indent+cur)
	}
	return lines
}

func triageText(t *Triage) []string {
	if t == nil {
		return nil
	}
	head := "ROUTINE CONFIRMATION — no new findings"
	if t.NotifyAll {
		head = "NEW FINDINGS — PLEASE READ"
	}
	lines := []string{"", "  " + head, "  (" + t.Summary + ")", ""}
	for _, g := range t.groups() {
		lines = append(lines, "  "+g.Title+":")
		for _, item := range g.Items {
			wrapped := tw.Wrap(item, W-6, "      ")
			lines = append(lines, "    - "+wrapped[0])
			lines = append(lines, wrapped[1:]...)
		}
		lines = append(lines, "")
	}
	return lines
}

func summaryText(m *Model) []string {
	lines := []string{"", "  AT A GLANCE", "  " + strings.Repeat("─", W-4)}
	for _, sec := range m.Applicable() {
		p := Pal(sec.Status)
		flag := ""
		if n := sec.AttentionRows(); n > 0 {
			flag = fmt.Sprintf("  (%d to review)", n)
		}
		lines = append(lines, fmt.Sprintf("  %s  %s %s%s", p.Sym, tw.Pad(sec.Title, labelW), p.Word, flag))
	}
	if skipped := m.Skipped(); len(skipped) > 0 {
		for _, l := range tw.Wrap("Not present on this host: "+strings.Join(skipped, ", "), W-4, "") {
			lines = append(lines, "  "+l)
		}
	}
	return append(lines, "")
}

func legendText() string {
	var marks []string
	for _, st := range []check.Status{check.OK, check.Info, check.Caution, check.Unhealthy} {
		p := Pal(st)
		marks = append(marks, p.Sym+" "+p.Word)
	}
	return "  Legend: " + strings.Join(marks, "   ")
}

// rowLines renders one row inside W columns.
//
// A row that is fine gets exactly one line: an inventory entry does not earn
// three lines of wrapped Kubernetes container name. Flagged rows wrap in full
// and keep their detail, because that is where a reader is going to look.
func rowLines(r check.Row) []string {
	sym := Pal(r.Status).Sym
	label := tw.Truncate(r.Label, labelW)
	value := r.Value
	if r.Meter != nil {
		value = tw.Meter(*r.Meter) + " " + value
	}
	// Inventory rows carry no label; padding 30 blank columns before the
	// value just leaves a hole in the middle of the report.
	prefix := "  " + sym + "  "
	if label != "" {
		prefix += tw.Pad(label, labelW) + " "
	}
	indent := strings.Repeat(" ", tw.Len(prefix))
	valW := W - tw.Len(prefix)

	if !r.Status.Flagged() {
		out := []string{prefix + tw.Truncate(value, valW)}
		if r.Delta != "" {
			for _, c := range tw.Wrap(r.Delta, valW, "") {
				out = append(out, indent+c)
			}
		}
		return out
	}

	var out []string
	for i, chunk := range tw.Wrap(value, valW, indent) {
		if i == 0 {
			chunk = prefix + chunk
		}
		out = append(out, chunk)
	}
	// Delta and detail get their own lines. Appending them to the value let
	// the wrapper split them mid-phrase ("...since last / run").
	extras := []string{r.Delta}
	if r.Detail != "" {
		extras = append(extras, "("+r.Detail+")")
	}
	for _, extra := range extras {
		if extra == "" {
			continue
		}
		for _, c := range tw.Wrap(extra, valW, "") {
			out = append(out, indent+c)
		}
	}
	return out
}
