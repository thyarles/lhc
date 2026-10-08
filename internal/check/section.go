package check

import "github.com/thyarles/lhc/internal/host"

// Row is one line of a section.
type Row struct {
	Label  string   `json:"label"`
	Value  string   `json:"value"`
	Status Status   `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Meter  *float64 `json:"meter,omitempty"` // 0-100: draws a usage bar beside the value
	Delta  string   `json:"delta,omitempty"` // change since the previous run
	// Separator rows are sub-headings inside a section. They carry no
	// status of their own and never change the section's.
	Separator bool `json:"separator,omitempty"`
}

// Alert is a condition worth notifying a human about. The notifier decides
// who: CAUTION reaches the broad list only when it is new, UNHEALTHY is
// chased until it clears.
type Alert struct {
	Status Status `json:"status"`
	Msg    string `json:"msg"`
}

// Tool is a command a check would like to have. The package names differ per
// family; an empty one means "same as Name".
type Tool struct {
	Name     string `json:"name"`
	RHELPkg  string `json:"rhel_pkg,omitempty"`
	DebPkg   string `json:"deb_pkg,omitempty"`
	SUSEPkg  string `json:"suse_pkg,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// Package returns the package that provides the tool for a package manager
// (dnf, yum, apt-get or zypper).
func (t Tool) Package(pm string) string {
	pick := func(p string) string {
		if p == "" {
			return t.Name
		}
		return p
	}
	switch pm {
	case "apt-get":
		return pick(t.DebPkg)
	case "zypper":
		if t.SUSEPkg == "" {
			return pick(t.RHELPkg)
		}
		return t.SUSEPkg
	default:
		return pick(t.RHELPkg)
	}
}

// InstallCmd is the command that installs the tool on this host. With no
// package manager detected it spells out every family, so the advice is
// still usable.
func (t Tool) InstallCmd(pm string) string {
	if cmd := host.InstallCmd(pm, t.Package(pm)); cmd != "" {
		return cmd
	}
	return "yum install -y " + t.Package("yum") + "  OR  apt-get install -y " + t.Package("apt-get") +
		"  OR  zypper install " + t.Package("zypper")
}

// Section is what one check produces.
type Section struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	// Applicable is false when the subject of the check does not exist on
	// this host (no Docker, no fail2ban). Such sections collapse to a single
	// "not present" mention instead of each taking a whole panel.
	Applicable   bool    `json:"applicable"`
	Rows         []Row   `json:"rows"`
	Alerts       []Alert `json:"alerts,omitempty"`
	MissingTools []Tool  `json:"missing_tools,omitempty"`
	// Facts feed the vital signs strip. The report lists them once, at the
	// top, so they are not repeated inside the section's JSON.
	Facts []Fact `json:"-"`
	Err   error  `json:"-"`
}

// Fact is one vital sign: a number people look for first on any host
// (uptime, pending updates, the fullest disk). The key is the contract; the
// report decides the label and the order.
type Fact struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Status Status `json:"status"`
}

// NewSection starts an applicable, OK section.
func NewSection(name, title string) *Section {
	return &Section{Name: name, Title: title, Applicable: true, Rows: []Row{}}
}

// RowOpt adds an optional field to a row.
type RowOpt func(*Row)

// Detail adds the small explanatory line under a value.
func Detail(d string) RowOpt { return func(r *Row) { r.Detail = d } }

// Meter draws a 0-100 usage bar. Only where a percentage is the point of the
// row (disk, memory, CPU).
func Meter(pct float64) RowOpt { return func(r *Row) { r.Meter = &pct } }

// Delta records the signed change since the previous run.
func Delta(d string) RowOpt { return func(r *Row) { r.Delta = d } }

// Add appends a row and folds its status into the section's.
func (s *Section) Add(label, value string, st Status, opts ...RowOpt) {
	r := Row{Label: label, Value: value, Status: st}
	for _, o := range opts {
		o(&r)
	}
	s.Rows = append(s.Rows, r)
	s.Status = Worse(s.Status, st)
}

// Separator adds a sub-heading.
func (s *Section) Separator(title string) {
	s.Rows = append(s.Rows, Row{Label: title, Status: Info, Separator: true})
}

// NotApplicable marks the check as having nothing to inspect on this host.
func (s *Section) NotApplicable(reason string) {
	s.Applicable = false
	s.Add(s.Title, reason, Info)
}

// Alert records a condition worth notifying about and raises the section's
// status to match.
func (s *Section) Alert(st Status, msg string) {
	s.Alerts = append(s.Alerts, Alert{Status: st, Msg: msg})
	s.Status = Worse(s.Status, st)
}

// Fact records a vital sign. Unlike a row, it never changes the section's
// status and never raises an alert: the row it summarises already did.
func (s *Section) Fact(key, value string, st Status) {
	s.Facts = append(s.Facts, Fact{Key: key, Value: value, Status: st})
}

// NeedTool records a missing command, for `lhc tools`.
func (s *Section) NeedTool(t Tool) { s.MissingTools = append(s.MissingTools, t) }

// AttentionRows counts the rows a reader is expected to look at.
func (s *Section) AttentionRows() int {
	n := 0
	for _, r := range s.Rows {
		if !r.Separator && r.Status.Flagged() {
			n++
		}
	}
	return n
}

// AlertMsgs lists the alert messages, for tests and logs.
func (s *Section) AlertMsgs() []string {
	out := make([]string, len(s.Alerts))
	for i, a := range s.Alerts {
		out[i] = a.Msg
	}
	return out
}
