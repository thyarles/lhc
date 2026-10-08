package report

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

// Palette for one status.
//
// Status colours mean state and nothing else, and always ship with a glyph
// AND the word, so meaning never rests on hue alone. The hues (Dot) are mark
// colours, not text colours: amber on white is 1.79:1. So each status also
// carries a darker FG ink for text and a pale BG tint for chips; every FG/BG
// pair passes WCAG AA (a test measures it). INFO deliberately has no hue: it
// is not a state anyone acts on, and colouring it competes with the rows that
// matter.
type Palette struct {
	FG, BG, Dot template.CSS
	Word, Sym   string
}

var palettes = map[check.Status]Palette{
	check.OK:        {FG: "#0a7a0a", BG: "#e8f6e8", Dot: "#0ca30c", Word: "OK", Sym: "✓"},
	check.Info:      {FG: "#52514e", BG: "#f1f1ef", Dot: "#8a8985", Word: "INFO", Sym: "·"},
	check.Caution:   {FG: "#8a5a00", BG: "#fdf3dd", Dot: "#fab219", Word: "CAUTION", Sym: "!"},
	check.Unhealthy: {FG: "#b02b2b", BG: "#fbe9e9", Dot: "#d03b3b", Word: "UNHEALTHY", Sym: "✕"},
}

// Pal returns the palette for a status.
func Pal(s check.Status) Palette {
	if p, ok := palettes[s]; ok {
		return p
	}
	return palettes[check.OK]
}

// Theme is the neutral ink and surfaces.
type Theme struct{ Ink, Muted, Rule, Surface, Surface2 template.CSS }

var theme = Theme{Ink: "#1a1a19", Muted: "#6b6a66", Rule: "#e3e3e0", Surface: "#ffffff", Surface2: "#f7f7f5"}

//go:embed report.html.tmpl
var htmlSource string

var htmlTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"pal": Pal,
	"T":   func() Theme { return theme },
	"pct": func(p *float64) template.CSS {
		return template.CSS(fmt.Sprintf("%.0f%%", math.Max(0, math.Min(100, *p)))) //nolint:gosec // a formatted number, never user text
	},
	// The at-a-glance grid: two columns, the first one taking the odd one.
	"columns": func(secs []*check.Section) [][]*check.Section {
		half := (len(secs) + 1) / 2
		return [][]*check.Section{secs[:half], secs[half:]}
	},
	"groups":   func(t *Triage) []triageGroup { return t.groups() },
	"statuses": func() []check.Status { return []check.Status{check.OK, check.Info, check.Caution, check.Unhealthy} },
	"join":     strings.Join,
	"vital":    VitalText,
}).Parse(htmlSource))

// HTML renders the e-mail body / standalone page.
func HTML(m *Model) (string, error) {
	var buf bytes.Buffer
	if err := htmlTmpl.Execute(&buf, m); err != nil {
		return "", err
	}
	return buf.String(), nil
}
