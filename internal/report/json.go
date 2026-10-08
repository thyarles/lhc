package report

import (
	"encoding/json"
	"time"

	"github.com/thyarles/lhc/internal/check"
)

// SchemaVersion of the JSON report. Bump on an incompatible change.
const SchemaVersion = 1

type jsonReport struct {
	Schema      int              `json:"schema"`
	Version     string           `json:"version"`
	Host        string           `json:"host"`
	GeneratedAt time.Time        `json:"generated_at"`
	Overall     check.Status     `json:"overall"`
	Triage      *Triage          `json:"triage"`
	Sections    []*check.Section `json:"sections"`
}

// JSON renders the machine-readable report.
func JSON(m *Model) ([]byte, error) {
	secs := m.Sections
	if secs == nil {
		secs = []*check.Section{}
	}
	// Lists are always arrays, never null, so consumers need no nil checks.
	var tr *Triage
	if m.Triage != nil {
		c := *m.Triage
		for _, l := range []*[]string{&c.New, &c.Escalated, &c.Reminders, &c.Ongoing, &c.Resolved} {
			if *l == nil {
				*l = []string{}
			}
		}
		tr = &c
	}
	b, err := json.MarshalIndent(jsonReport{
		Schema: SchemaVersion, Version: m.Version, Host: m.Host,
		GeneratedAt: m.Generated, Overall: m.Overall, Triage: tr, Sections: secs,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
