// Package state keeps what one run needs to remember for the next: the
// baselines the change-detecting checks diff against, and the alert history
// that decides who gets interrupted.
//
// One JSON file per owner (a check, or "alerts"), holding several keys:
//
//	{"version": 1, "saved_at": "...", "data": {"ports": [...], ...}}
//
// A missing, corrupt or older-format file reads as "nothing saved", which
// every caller already handles as a first run. That is deliberate: a damaged
// baseline must cost one re-baseline, never a crash.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Version of the envelope. Bump it when a stored shape changes incompatibly;
// old files are then ignored rather than misread.
const Version = 1

// Store is one owner's slice of the state directory.
type Store interface {
	// Load decodes key into out. It returns false when nothing usable is
	// stored: absent, unreadable, corrupt or written by another version.
	Load(key string, out any) bool
	// Save records v under key.
	Save(key string, v any) error
}

type envelope struct {
	Version int                        `json:"version"`
	SavedAt time.Time                  `json:"saved_at"`
	Data    map[string]json.RawMessage `json:"data"`
}

// Dir hands out one Store per owner, all under the same directory.
type Dir struct {
	Path string
	// ReadOnly turns every Save into a no-op. `lhc report` and `lhc run
	// --dry-run` set it so that previewing a report does not consume the
	// baselines the scheduled run diffs against.
	ReadOnly bool
	// Deferred holds every Save in memory until Flush. A delivering run sets
	// it so that the baselines advance together with the alert history, only
	// once the report reached someone. Writing them as each check finished
	// let a failed delivery, or an interrupted run, consume a change that
	// nobody was ever told about.
	Deferred bool
	Now      func() time.Time

	mu      sync.Mutex
	pending map[string]map[string]json.RawMessage // owner → key → value
}

// For returns the Store for one owner, e.g. a check's name.
func (d *Dir) For(owner string) Store { return &fileStore{dir: d, owner: owner} }

type fileStore struct {
	dir   *Dir
	owner string
}

func (f *fileStore) file() string { return filepath.Join(f.dir.Path, f.owner+".json") }

func (f *fileStore) read() envelope {
	var env envelope
	b, err := os.ReadFile(f.file())
	if err != nil || json.Unmarshal(b, &env) != nil || env.Version != Version || env.Data == nil {
		return envelope{Version: Version, Data: map[string]json.RawMessage{}}
	}
	return env
}

func (f *fileStore) Load(key string, out any) bool {
	f.dir.mu.Lock()
	defer f.dir.mu.Unlock()
	raw, ok := f.dir.pending[f.owner][key]
	if !ok {
		raw, ok = f.read().Data[key]
	}
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

func (f *fileStore) Save(key string, v any) error {
	if f.dir.ReadOnly {
		return nil
	}
	f.dir.mu.Lock()
	defer f.dir.mu.Unlock()
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("state %s/%s: %w", f.owner, key, err)
	}
	if f.dir.Deferred {
		if f.dir.pending == nil {
			f.dir.pending = map[string]map[string]json.RawMessage{}
		}
		if f.dir.pending[f.owner] == nil {
			f.dir.pending[f.owner] = map[string]json.RawMessage{}
		}
		f.dir.pending[f.owner][key] = raw
		return nil
	}
	return f.write(map[string]json.RawMessage{key: raw})
}

// write merges data into the owner's file. The caller holds dir.mu.
func (f *fileStore) write(data map[string]json.RawMessage) error {
	env := f.read()
	maps.Copy(env.Data, data)
	now := time.Now
	if f.dir.Now != nil {
		now = f.dir.Now
	}
	env.SavedAt = now().UTC().Truncate(time.Second)
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir.Path, 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(f.file(), append(b, '\n'), 0o600)
}

// Flush writes what a Deferred Dir has held back, one file per owner, and
// forgets it. Every owner is attempted; the first error is returned.
func (d *Dir) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var first error
	for _, owner := range slices.Sorted(maps.Keys(d.pending)) {
		if err := (&fileStore{dir: d, owner: owner}).write(d.pending[owner]); err != nil && first == nil {
			first = fmt.Errorf("state %s: %w", owner, err)
		}
	}
	d.pending = nil
	return first
}

// WriteFileAtomic writes through a temporary file in the same directory,
// fsyncs it and renames it over path, so a crash or a full disk leaves either
// the old file or the new one, never half of each.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(e error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return e
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Rename(name, path); err != nil {
		return cleanup(err)
	}
	return nil
}

// Memory is an in-memory Store for tests.
type Memory struct {
	mu   sync.Mutex
	Data map[string]json.RawMessage
}

func NewMemory() *Memory { return &Memory{Data: map[string]json.RawMessage{}} }

func (m *Memory) Load(key string, out any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.Data[key]
	return ok && json.Unmarshal(raw, out) == nil
}

func (m *Memory) Save(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Data[key] = raw
	return nil
}

// Has reports whether key was saved. For tests.
func (m *Memory) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Data[key]
	return ok
}

// ErrLocked is returned by Lock when another run holds the lock.
var ErrLocked = errors.New("another lhc run is in progress")
