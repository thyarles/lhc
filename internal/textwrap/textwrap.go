// Package textwrap holds the fixed-width helpers the text report is built
// from. Widths are counted in runes, so "…" and "✓" are one column each.
package textwrap

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Len is the width of s in columns.
func Len(s string) int { return utf8.RuneCountInString(s) }

// Truncate cuts s to width columns, ending with "…" when it had to cut.
func Truncate(s string, width int) string {
	if Len(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:width-1]) + "…"
}

// Pad left-aligns s in width columns.
func Pad(s string, width int) string {
	if n := Len(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// Wrap hard-wraps text to width columns. Words longer than the column are
// split with "…" rather than allowed to overflow: the old Python renderer
// declared a 76-column report and then emitted 155-column lines whenever a
// Kubernetes container name showed up, so every mail client re-wrapped it
// and the alignment fell apart. Lines after the first get indent.
func Wrap(text string, width int, indent string) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(text) {
		for Len(word) > width {
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
			r := []rune(word)
			lines = append(lines, string(r[:width-1])+"…")
			word = string(r[width-1:])
		}
		switch {
		case cur == "":
			cur = word
		case Len(cur)+1+Len(word) <= width:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = word
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	for i := 1; i < len(lines); i++ {
		lines[i] = indent + lines[i]
	}
	return lines
}

// Meter is a coarse ten-cell bar: enough to read magnitude at a glance in a
// fixed-width body. Out-of-range values are clamped.
func Meter(pct float64) string {
	const width = 10
	pct = math.Max(0, math.Min(100, pct))
	filled := int(math.RoundToEven(pct / 100 * width))
	return "[" + strings.Repeat("#", filled) + strings.Repeat(".", width-filled) + "]"
}

// FitHost composes a header or footer line inside width columns. The
// hostname is the only elastic part, and a cloud FQDN is routinely 60+
// characters on its own. The leading label is what identifies the machine,
// so it is the domain tail that gets dropped.
func FitHost(template, host string, width int) string {
	line := strings.ReplaceAll(template, "{host}", host)
	over := Len(line) - width
	if over <= 0 {
		return line
	}
	keep := max(Len(host)-over-1, 8)
	return strings.ReplaceAll(template, "{host}", string([]rune(host)[:min(keep, Len(host))])+"…")
}
