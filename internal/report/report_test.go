package report

import (
	"encoding/json"
	"flag"
	stdhtml "html"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
	tw "github.com/thyarles/lhc-go/internal/textwrap"
)

// These pin the properties that make the report readable — worst-first
// ordering, a bounded line width, colour never carrying meaning alone —
// rather than exact wording, which the golden files cover.

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

type row struct {
	label, value string
	st           check.Status
}

func sec(title string, rows ...row) *check.Section {
	s := check.NewSection(strings.ToLower(title), title)
	for _, r := range rows {
		s.Add(r.label, r.value, r.st)
	}
	return s
}

func notApplicable(title string) *check.Section {
	s := check.NewSection(strings.ToLower(title), title)
	s.NotApplicable("Not installed")
	return s
}

var when = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)

func model(host string, triage *Triage, secs ...*check.Section) *Model {
	overall := check.OK
	for _, s := range secs {
		overall = check.Worse(overall, s.Status)
	}
	return &Model{Host: host, Version: "1.2.3", Generated: when, Overall: overall, Sections: Order(secs), Triage: triage}
}

func text(secs ...*check.Section) string { return Text(model("web01.example.com", nil, secs...)) }

func html(t *testing.T, m *Model) string {
	t.Helper()
	out, err := HTML(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func titles(secs []*check.Section) []string {
	var out []string
	for _, s := range secs {
		out = append(out, s.Title)
	}
	return out
}

// ── worst first ─────────────────────────────────────────────────────────────

func TestSectionsAreOrderedWorstFirst(t *testing.T) {
	got := titles(Order([]*check.Section{
		sec("Network", row{"a", "1", check.OK}),
		sec("Disk", row{"b", "2", check.Caution}),
		sec("Services", row{"c", "3", check.Unhealthy}),
		sec("Ports", row{"d", "4", check.Info}),
	}))
	if !slices.Equal(got, []string{"Services", "Disk", "Ports", "Network"}) {
		t.Fatal(got)
	}
}

func TestOrderingIsStableWithinASeverityBand(t *testing.T) {
	ok := row{"x", "1", check.OK}
	if got := titles(Order([]*check.Section{sec("A", ok), sec("B", ok), sec("C", ok)})); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatal(got)
	}
}

func TestTheWorstSectionAppearsFirstInTheOutput(t *testing.T) {
	out := text(sec("Network Aardvark", row{"a", "1", check.OK}), sec("Zebra Services", row{"c", "3", check.Unhealthy}))
	body := strings.SplitN(out, "Legend:", 2)[1]
	if strings.Index(body, "Zebra Services") > strings.Index(body, "Network Aardvark") {
		t.Fatal("worst section is not first")
	}
}

// ── line-width discipline ───────────────────────────────────────────────────

const (
	longLabel = "k8s_POD_helm-install-traefik-crd-wzsss_kube-system_429d29a9-55c3-4354-855b"
	longValue = "Exited (0) About an hour ago  [rancher/mirrored-pause:3.6-with-a-very-long-tag]"
	// Cloud FQDNs really look like this; a CI runner first caught the
	// header overflow with one.
	longHost = "srv0123456789.subdomain-that-goes-on.internal.example.cloudapp.net"
)

func TestNoLineExceedsTheDeclaredWidth(t *testing.T) {
	s := sec("Docker", row{longLabel, longValue, check.Info}, row{"short", "value", check.OK}, row{"flagged", longValue + " " + longLabel, check.Caution})
	tr := &Triage{NotifyAll: true, Summary: "1 new", New: []string{"Kubernetes pods: " + longLabel + ", " + longLabel}}
	for _, l := range strings.Split(Text(model(longHost, tr, s)), "\n") {
		if tw.Len(l) > W {
			t.Fatalf("%d cols: %q", tw.Len(l), l)
		}
	}
}

func TestALongHostnameKeepsThePartThatIdentifiesTheMachine(t *testing.T) {
	if !strings.Contains(Text(model(longHost, nil, sec("Docker", row{"a", "1", check.OK}))), "srv0123456789") {
		t.Fatal("leading label lost")
	}
}

func TestAShortHostnameIsNeverTruncated(t *testing.T) {
	if !strings.Contains(text(sec("Docker", row{"a", "1", check.OK})), "web01.example.com") {
		t.Fatal("short host truncated")
	}
}

func TestFlaggedRowsWrapTheirValueInFull(t *testing.T) {
	var wrapped bool
	for _, l := range strings.Split(text(sec("Docker", row{"name", longValue, check.Caution})), "\n") {
		if strings.HasPrefix(l, strings.Repeat(" ", 20)) && strings.Contains(l, "rancher") {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("a flagged value should continue on an indented line")
	}
}

func TestHealthyRowsGetExactlyOneLine(t *testing.T) {
	body := strings.SplitN(text(sec("Docker", row{"name", longValue, check.Info})), "Legend:", 2)[1]
	var rows []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, "name") && strings.Contains(l, "Exited") {
			rows = append(rows, l)
		}
	}
	if len(rows) != 1 || !strings.HasSuffix(rows[0], "…") {
		t.Fatalf("an INFO row should occupy a single truncated line: %q", rows)
	}
}

// ── meters and deltas ───────────────────────────────────────────────────────

func meterSection() *check.Section {
	s := check.NewSection("disk", "Disk Usage")
	s.Add("/var", "91% used", check.Caution, check.Meter(91), check.Delta("+3.0 pts since last run"))
	return s
}

func TestHTMLMeterRendersForRowsThatHaveOne(t *testing.T) {
	if !strings.Contains(html(t, model("h", nil, meterSection())), "width:91%") {
		t.Fatal("no meter")
	}
}

func TestRowsWithoutAMeterRenderNoBar(t *testing.T) {
	out := html(t, model("h", nil, sec("Info", row{"Kernel", "6.6.0", check.OK})))
	after := strings.SplitN(out, "Kernel", 2)[1]
	if strings.Contains(after[:min(400, len(after))], "width:") {
		t.Fatal("bar rendered without a meter")
	}
}

func TestDeltaIsShownNextToTheValue(t *testing.T) {
	m := model("h", nil, meterSection())
	if !strings.Contains(stdhtml.UnescapeString(html(t, m)), "+3.0 pts since last run") || !strings.Contains(Text(m), "+3.0 pts since last run") {
		t.Fatal("delta missing")
	}
}

// ── not-applicable sections ─────────────────────────────────────────────────

func TestNotApplicableSectionsAreCollapsedToOneLine(t *testing.T) {
	out := text(sec("Disk Usage", row{"/", "40%", check.OK}), notApplicable("Docker Containers"))
	if !strings.Contains(out, "Not present on this host: Docker Containers") || strings.Count(out, "Docker Containers") != 1 {
		t.Fatal(out)
	}
}

func TestNotApplicableSectionsAreCollapsedInHTML(t *testing.T) {
	out := html(t, model("h", nil, sec("Disk Usage", row{"/", "40%", check.OK}), notApplicable("Docker Containers")))
	if !strings.Contains(out, "Not present on this host") || strings.Count(out, "Docker Containers") != 1 {
		t.Fatal("not collapsed")
	}
}

// ── summary grid ────────────────────────────────────────────────────────────

func summary(out string) string {
	return strings.SplitN(strings.SplitN(out, "AT A GLANCE", 2)[1], "Legend:", 2)[0]
}

func TestSummaryListsEveryApplicableSection(t *testing.T) {
	s := summary(text(sec("Disk Usage", row{"/", "40%", check.OK}), sec("Memory Usage", row{"Mem", "60%", check.OK})))
	if !strings.Contains(s, "Disk Usage") || !strings.Contains(s, "Memory Usage") {
		t.Fatal(s)
	}
}

func TestSummaryCountsTheRowsNeedingReview(t *testing.T) {
	s := summary(text(sec("Disk", row{"/var", "91%", check.Caution}, row{"/tmp", "97%", check.Unhealthy}, row{"/", "10%", check.OK})))
	if !strings.Contains(s, "2 to review") {
		t.Fatal(s)
	}
}

func TestHTMLSummaryIsPresent(t *testing.T) {
	if !strings.Contains(html(t, model("h", nil, sec("Disk", row{"/", "40%", check.OK}))), "At a glance") {
		t.Fatal("no summary")
	}
}

// ── colour never carries meaning alone ──────────────────────────────────────

var allStatuses = []check.Status{check.OK, check.Info, check.Caution, check.Unhealthy}

func TestEveryStatusHasAGlyphAndAWord(t *testing.T) {
	for _, st := range allStatuses {
		p := Pal(st)
		if p.Sym == "" || p.Word == "" || p.Word != strings.ToUpper(p.Word) {
			t.Errorf("%v: %+v", st, p)
		}
	}
}

func TestHTMLStatusChipsPairColourWithTheWord(t *testing.T) {
	for _, st := range allStatuses {
		if !strings.Contains(html(t, model("h", nil, sec("T", row{"row", "value", st}))), Pal(st).Word) {
			t.Errorf("%v: word missing", st)
		}
	}
}

func TestTextReportHasALegend(t *testing.T) {
	out := text(sec("Disk", row{"/", "40%", check.OK}))
	if !strings.Contains(out, "Legend:") {
		t.Fatal("no legend")
	}
	for _, st := range allStatuses {
		if !strings.Contains(out, Pal(st).Word) {
			t.Errorf("%v missing from legend", st)
		}
	}
}

func TestOKRowsAreNotPaintedWithAStatusColour(t *testing.T) {
	// 163 green rows are what made the 4 amber ones invisible.
	out := html(t, model("h", nil, sec("T", row{"fine", "all good", check.OK})))
	after := strings.SplitN(out, "fine", 2)[1]
	if strings.Contains(after[:300], string(Pal(check.OK).BG)) {
		t.Fatal("OK row carries a tinted background")
	}
}

func TestFlaggedRowsAreTinted(t *testing.T) {
	if !strings.Contains(html(t, model("h", nil, sec("T", row{"bad", "91% used", check.Caution}))), string(Pal(check.Caution).BG)) {
		t.Fatal("flagged row not tinted")
	}
}

// ── accessibility of the palette ────────────────────────────────────────────

func luminance(hex string) float64 {
	h := strings.TrimPrefix(hex, "#")
	var ch [3]float64
	for i := range 3 {
		v, _ := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.04045 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func TestStatusTextMeetsWCAGAA(t *testing.T) {
	// Amber on white is 1.79:1 — unreadable. Each status carries a darker ink.
	for _, st := range allStatuses {
		p := Pal(st)
		if c := contrast(string(p.FG), string(p.BG)); c < 4.5 {
			t.Errorf("%v on its chip: %.2f", st, c)
		}
		if c := contrast(string(p.FG), "#ffffff"); c < 4.5 {
			t.Errorf("%v on the page: %.2f", st, c)
		}
	}
}

func TestEveryColourTokenIsAValidHex(t *testing.T) {
	re := regexp.MustCompile(`^#[0-9a-f]{6}$`)
	for st, p := range palettes {
		for _, c := range []string{string(p.FG), string(p.BG), string(p.Dot)} {
			if !re.MatchString(c) {
				t.Errorf("%v: %q", st, c)
			}
		}
	}
}

// ── html hygiene ────────────────────────────────────────────────────────────

func TestHTMLOptsOutOfClientSideAutoDarkening(t *testing.T) {
	// A half-dark theme is worse than none.
	out := html(t, model("h", nil, sec("T", row{"a", "1", check.OK})))
	if !strings.Contains(out, `name="color-scheme" content="only light"`) || !strings.Contains(out, "color-scheme: only light") {
		t.Fatal("light lock missing")
	}
	if strings.Contains(out, "prefers-color-scheme") {
		t.Fatal("partial dark theme")
	}
}

func TestNoElementCarriesTextColourWithoutABackground(t *testing.T) {
	// An inherited background is what disappears when a client force-inverts.
	s := check.NewSection("disk", "Disk Usage")
	s.Add("/var", "91% used", check.Caution, check.Meter(91))
	s.Add("/", "40% used", check.OK)
	tr := &Triage{NotifyAll: true, Summary: "1 new", New: []string{"x"}, Resolved: []string{"y"}}
	body := strings.SplitN(html(t, model("h", tr, s, notApplicable("Docker"))), "<body", 2)[1]
	tag := regexp.MustCompile(`<(td|tr|div|li)\b([^>]*)>`)
	style := regexp.MustCompile(`style="([^"]*)"`)
	color := regexp.MustCompile(`(^|;)\s*color:`)
	var naked []string
	for _, m := range tag.FindAllStringSubmatch(body, -1) {
		st := style.FindStringSubmatch(m[2])
		if st == nil || !color.MatchString(st[1]) {
			continue
		}
		hasBG := strings.Contains(st[1], "background")
		hasClass := strings.Contains(m[2], "hc-ink") || strings.Contains(m[2], "hc-muted") || strings.Contains(m[2], "hc-row")
		if !hasBG && !hasClass {
			naked = append(naked, "<"+m[1]+"> "+st[1])
		}
	}
	if len(naked) > 0 {
		t.Fatalf("these would vanish on a force-inverting client: %q", naked)
	}
}

func TestUnflaggedRowsArePinnedWhiteButFlaggedRowsKeepTheirTint(t *testing.T) {
	out := html(t, model("h", nil, sec("T", row{"fine", "ok", check.OK}, row{"bad", "91%", check.Caution})))
	if !strings.Contains(out, `class="hc-row"`) || !strings.Contains(out, `class="hc-flag"`) || !strings.Contains(out, string(Pal(check.Caution).BG)) {
		t.Fatal("row classes wrong")
	}
}

func TestUnlabelledRowsDoNotPadAHoleBeforeTheValue(t *testing.T) {
	for _, l := range strings.Split(text(sec("Listening Sockets", row{"", "tcp *:443", check.Info})), "\n") {
		if strings.Contains(l, "tcp *:443") && l != "  ·  tcp *:443" {
			t.Fatalf("%q", l)
		}
	}
}

func TestLabelledRowsStillAlignInAColumn(t *testing.T) {
	var cols []int
	for _, l := range strings.Split(text(sec("T", row{"Kernel", "6.6.0", check.OK}, row{"Uptime", "3 hours", check.OK})), "\n") {
		if i := strings.Index(l, "6.6.0"); i >= 0 {
			cols = append(cols, i)
		}
		if i := strings.Index(l, "3 hours"); i >= 0 {
			cols = append(cols, i)
		}
	}
	if len(cols) != 2 || cols[0] != cols[1] {
		t.Fatalf("columns %v", cols)
	}
}

// ── triage ──────────────────────────────────────────────────────────────────

func diskSections() []*check.Section {
	return []*check.Section{sec("Disk Usage", row{"/var", "91% used", check.Caution})}
}

const diskMsg = "Disk /var at 91%"

func TestHTMLMarksANewFindingAsNeedingAttention(t *testing.T) {
	out := html(t, model("h", &Triage{NotifyAll: true, New: []string{diskMsg}}, diskSections()...))
	if !strings.Contains(strings.ToLower(out), "new findings") || !strings.Contains(out, "Needs attention") || !strings.Contains(out, diskMsg) {
		t.Fatal("triage missing")
	}
}

func TestHTMLMarksAnUnchangedFindingAsNoAction(t *testing.T) {
	out := html(t, model("h", &Triage{Ongoing: []string{diskMsg}}, diskSections()...))
	if !strings.Contains(out, "Routine confirmation") || !strings.Contains(out, "no action implied") {
		t.Fatal("routine block missing")
	}
}

func TestHTMLSaysSoWhenThereIsNothingToReport(t *testing.T) {
	if !strings.Contains(html(t, model("h", &Triage{}, diskSections()...)), "Nothing requiring attention") {
		t.Fatal("quiet run not described")
	}
}

func TestHTMLListsResolvedConditions(t *testing.T) {
	if !strings.Contains(html(t, model("h", &Triage{Resolved: []string{diskMsg}}, diskSections()...)), "Cleared since last run") {
		t.Fatal("resolved missing")
	}
}

func TestHTMLEscapesEverythingAHostCanInfluence(t *testing.T) {
	evil := `<script>alert("x")</script>`
	s := sec(evil, row{evil, evil, check.Caution})
	s.Rows[0].Detail = evil
	s.Separator(evil)
	out := html(t, model(evil, &Triage{NotifyAll: true, New: []string{evil}}, s, notApplicable(evil)))
	if strings.Contains(out, "<script>") || !strings.Contains(out, "&lt;script&gt;") {
		t.Fatal("unescaped markup")
	}
}

func TestHTMLWithoutATriageStillRenders(t *testing.T) {
	out := html(t, model("h", nil, diskSections()...))
	if !strings.Contains(out, "CAUTION") || !strings.Contains(out, "Disk Usage") || strings.Contains(out, "Needs attention") {
		t.Fatal("no-triage render wrong")
	}
}

func TestTextReportLeadsWithTheTriageSummary(t *testing.T) {
	out := Text(model("h", &Triage{NotifyAll: true, New: []string{diskMsg}}, diskSections()...))
	lines := strings.Split(out, "\n")
	if !slices.ContainsFunc(lines[:8], func(l string) bool { return strings.Contains(l, "NEW FINDINGS") }) || !strings.Contains(out, diskMsg) {
		t.Fatal(out)
	}
}

func TestTextReportMarksARoutineRun(t *testing.T) {
	out := Text(model("h", &Triage{Ongoing: []string{diskMsg}}, diskSections()...))
	if !strings.Contains(out, "ROUTINE CONFIRMATION") || !strings.Contains(out, "no action implied") {
		t.Fatal(out)
	}
}

func TestEveryTriageGroupAppearsWhenPopulated(t *testing.T) {
	tr := &Triage{NotifyAll: true, New: []string{"a new thing"}, Escalated: []string{"a worse thing"},
		Reminders: []string{"an old thing"}, Ongoing: []string{"a known thing"}, Resolved: []string{"a fixed thing"}}
	out := Text(model("h", tr, diskSections()...))
	for _, p := range []string{"a new thing", "a worse thing", "an old thing", "a known thing", "a fixed thing"} {
		if !strings.Contains(out, p) {
			t.Errorf("%q missing", p)
		}
	}
}

// ── JSON ────────────────────────────────────────────────────────────────────

func TestJSONHasTheDocumentedShape(t *testing.T) {
	b, err := JSON(model("web01", &Triage{New: []string{diskMsg}}, append(diskSections(), notApplicable("Docker"))...))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema", "version", "host", "generated_at", "overall", "triage", "sections"} {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q missing", k)
		}
	}
	if got["overall"] != "caution" || got["schema"] != float64(1) {
		t.Errorf("overall/schema: %v %v", got["overall"], got["schema"])
	}
	secs := got["sections"].([]any)
	first := secs[0].(map[string]any)
	if first["name"] != "disk usage" || first["status"] != "caution" || first["applicable"] != true {
		t.Errorf("first section: %v", first)
	}
}

// ── golden files ────────────────────────────────────────────────────────────

func goldenModel() *Model {
	sys := check.NewSection("system", "System Information")
	sys.Add("Hostname", "web01.example.com", check.OK)
	sys.Add("Kernel", "5.14.0-427.el9.x86_64", check.OK)
	disk := check.NewSection("disk", "Disk Usage")
	disk.Add("/", "41.0% used  (8 GB of 20 GB, 11 GB free)", check.OK, check.Meter(41))
	disk.Add("/var", "91.0% used  (91 GB of 100 GB, 8 GB free)", check.Caution, check.Meter(91),
		check.Delta("+3.0 pts since last run"))
	disk.Alert(check.Caution, "Disk /var at 91%")
	ports := check.NewSection("ports", "Listening Ports")
	ports.Separator("Listening Sockets")
	ports.Add("", "tcp 0.0.0.0:22 users:((\"sshd\"))", check.Info)
	docker := check.NewSection("docker", "Docker Containers")
	docker.NotApplicable("Not installed (optional)")
	return model("web01.example.com", &Triage{NotifyAll: true, Summary: "1 new", New: []string{"Disk /var at 91%"},
		Resolved: []string{"Service nginx.service failed"}}, sys, disk, ports, docker)
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/report -update to create it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the golden file; if the change is intended, run: go test ./internal/report -update", name)
	}
}

func TestGoldenText(t *testing.T) { golden(t, "report.txt", Text(goldenModel())) }

func TestGoldenHTML(t *testing.T) { golden(t, "report.html", html(t, goldenModel())) }

func TestGoldenJSON(t *testing.T) {
	b, err := JSON(goldenModel())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "report.json", string(b))
}
