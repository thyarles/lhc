package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/state"
)

// Store keeps the latest report of each host: <dir>/<host>.json.
type Store struct{ Dir string }

// Stored is one host's file.
type Stored struct {
	ReceivedAt time.Time       `json:"received_at"`
	RemoteAddr string          `json:"remote_addr"`
	Report     json.RawMessage `json:"report"`
}

// summary is the part of a report the store reads back.
type summary struct {
	Schema      int          `json:"schema"`
	Host        string       `json:"host"`
	Overall     string       `json:"overall"`
	Version     string       `json:"version"`
	GeneratedAt time.Time    `json:"generated_at"`
	Vitals      []check.Fact `json:"vitals"`
	Sections    []struct {
		Name   string        `json:"name"`
		Rows   []check.Row   `json:"rows"`
		Alerts []check.Alert `json:"alerts"`
	} `json:"sections"`
}

// A collector's own report, with digest_mode one, has a section saying
// whether it could reach the relay that night (see cli's relaySection).
const (
	RelaySection = "hub"
	RelayRow     = "Digest Relay"
	RelayAlert   = "Digest relay unreachable from this collector"
)

func (s Store) path(host string) (string, error) {
	if !ValidHost(host) {
		return "", fmt.Errorf("%q is not a host name", host)
	}
	return filepath.Join(s.Dir, host+".json"), nil
}

// Put replaces a host's latest report.
func (s Store) Put(host string, st Stored) error {
	p, err := s.path(host)
	if err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	return state.WriteFileAtomic(p, b, 0o600)
}

// Entry is one host as the collector last heard from it.
type Entry struct {
	Host       string
	ReceivedAt time.Time
	Generated  time.Time
	Overall    string
	Version    string // lhc on the host; "" in a report from before it was sent
	// A collector's relay check, from its own report: RelayChecked when
	// the report has one, RelayErr when it failed.
	RelayChecked bool
	RelayErr     string
	Vitals       []check.Fact
	Alerts       []check.Alert // worst first
	Err          error         // the file could not be read
}

// List reads every host's file, sorted by host name. A missing store
// holds no hosts.
func (s Store) List() ([]Entry, error) {
	files, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, f := range files {
		host, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok || f.IsDir() || !ValidHost(host) {
			continue // temporary files of an atomic write start with a dot
		}
		e := Entry{Host: host}
		var st Stored
		var sum summary
		b, err := os.ReadFile(filepath.Join(s.Dir, f.Name()))
		if err == nil {
			err = json.Unmarshal(b, &st)
		}
		if err == nil {
			err = json.Unmarshal(st.Report, &sum)
		}
		e.ReceivedAt, e.Generated, e.Overall, e.Version, e.Vitals, e.Err = st.ReceivedAt, sum.GeneratedAt, sum.Overall, sum.Version, sum.Vitals, err
		for _, sec := range sum.Sections {
			e.Alerts = append(e.Alerts, sec.Alerts...)
			if sec.Name != RelaySection {
				continue
			}
			for _, r := range sec.Rows {
				if r.Label == RelayRow {
					e.RelayChecked = true
					if r.Status.Flagged() {
						e.RelayErr = r.Value
					}
				}
			}
		}
		slices.SortStableFunc(e.Alerts, func(a, b check.Alert) int { return int(b.Status) - int(a.Status) })
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Host, b.Host) })
	return out, nil
}

// Forget removes a host, so a decommissioned machine stops being listed.
func (s Store) Forget(host string) error {
	p, err := s.path(host)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: no report from this host", host)
	} else if err != nil {
		return err
	}
	return nil
}
