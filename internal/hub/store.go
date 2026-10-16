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
	Schema      int       `json:"schema"`
	Host        string    `json:"host"`
	Overall     string    `json:"overall"`
	GeneratedAt time.Time `json:"generated_at"`
}

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

// Entry is one row of `lhc hub status`.
type Entry struct {
	Host       string
	ReceivedAt time.Time
	Generated  time.Time
	Overall    string
	Err        error // the file could not be read
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
		e.ReceivedAt, e.Generated, e.Overall, e.Err = st.ReceivedAt, sum.GeneratedAt, sum.Overall, err
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
