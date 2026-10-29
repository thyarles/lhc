package hub

import (
	"context"
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

// With collector.digest_mode one, a hub sends one digest a day. The
// collectors take turns by name: the first sends at digest_time and tells
// the others; each next one sends takeover_after later only when it heard
// of no digest for that day. A lost notice can only cause a second mail,
// never none.

// NoticePath is where a collector says it sent the day's digest.
const NoticePath = "/v1/digest-sent"

// Notice is the sealed body of POST /v1/digest-sent.
type Notice struct {
	Schema int       `json:"schema"`
	Date   string    `json:"date"` // the digest's day, sender's local time: 2006-01-02
	By     string    `json:"by"`   // the collector that sent it
	At     time.Time `json:"at"`
}

// Order is the hub's collectors, sorted: self and the configured peers,
// lowercased, without repeats. Every collector computes the same order
// without any per-host setting. rank is self's place in it.
func Order(self string, peers []string) (order []string, rank int) {
	for _, h := range append([]string{self}, peers...) {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && !slices.Contains(order, h) {
			order = append(order, h)
		}
	}
	slices.Sort(order)
	return order, slices.Index(order, strings.ToLower(strings.TrimSpace(self)))
}

// Day is the date a digest slot belongs to.
func Day(slot time.Time) string { return slot.Format("2006-01-02") }

func (s Store) noticePath() string { return filepath.Join(s.Dir, ".digest-sent.json") }

// LoadNotice is the latest notice received, if any.
func (s Store) LoadNotice() (Notice, bool) {
	var n Notice
	b, err := os.ReadFile(s.noticePath())
	if err != nil || json.Unmarshal(b, &n) != nil || n.Date == "" {
		return Notice{}, false
	}
	return n, true
}

// SaveNotice keeps the first notice of each day: a second one (a duplicate
// mail after a lost notice) does not change who sent the day's digest.
func (s Store) SaveNotice(n Notice) error {
	if cur, ok := s.LoadNotice(); ok && cur.Date >= n.Date {
		return nil
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	return state.WriteFileAtomic(s.noticePath(), b, 0o600)
}

// errOldPeer is a collector that does not know the notice: an lhc from
// before digest_mode. It will send its own digest, as it always did.
var errOldPeer = errors.New("does not take digest notices (older lhc): it sends its own digest")

// Tell posts a notice to every collector, this one included.
func Tell(ctx context.Context, key []byte, collectors []string, n Notice, now time.Time, timeout time.Duration) []Outcome {
	body, err := json.Marshal(n)
	if err == nil {
		var e *Envelope
		if e, err = Seal(key, n.By, now, body); err == nil {
			body, err = json.Marshal(e)
		}
	}
	out := make([]Outcome, 0, len(collectors))
	for _, c := range collectors {
		o := Outcome{Collector: c, Err: err}
		if err == nil {
			o.Err = post(ctx, c, NoticePath, body, timeout)
		}
		out = append(out, o)
	}
	return out
}

// readNotice checks an opened notice against its envelope.
func readNotice(e *Envelope, plain []byte) (Notice, error) {
	var n Notice
	if err := json.Unmarshal(plain, &n); err != nil {
		return n, fmt.Errorf("%s: the notice is not JSON: %w", e.Host, err)
	}
	if n.Schema != 1 {
		return n, fmt.Errorf("%s: notice schema %d, want 1", e.Host, n.Schema)
	}
	if n.By != e.Host {
		return n, fmt.Errorf("%s: the notice inside is from %q", e.Host, n.By)
	}
	if _, err := time.Parse("2006-01-02", n.Date); err != nil {
		return n, fmt.Errorf("%s: notice date %q: %w", e.Host, n.Date, err)
	}
	return n, nil
}
