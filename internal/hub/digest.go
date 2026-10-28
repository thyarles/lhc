package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/report"
	"github.com/thyarles/lhc/internal/state"
)

// RetryEvery is how long a digest the relay refused waits for another try.
const RetryEvery = 30 * time.Minute

// DigestState is what the previous digest left for the next one, in
// <store>/.digest.json (a dot file, so List never reads it as a host).
type DigestState struct {
	// Slot is the digest_time of the last digest sent on schedule.
	Slot time.Time `json:"slot"`
	// Hosts is each host's status in that digest: ok, info, caution,
	// unhealthy, or silent. Nil before the first digest.
	Hosts map[string]string `json:"hosts"`
}

func (s Store) digestPath() string { return filepath.Join(s.Dir, ".digest.json") }

// LoadDigest reads the previous digest's state. A missing or unreadable
// file is "no digest yet".
func (s Store) LoadDigest() DigestState {
	var st DigestState
	b, err := os.ReadFile(s.digestPath())
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// SaveDigest records a digest that was sent.
func (s Store) SaveDigest(st DigestState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	return state.WriteFileAtomic(s.digestPath(), b, 0o600)
}

// DigestOptions is what a digest is built from besides the store.
type DigestOptions struct {
	Self        string   // this collector's host name
	Peers       []string // the other collectors
	SilentAfter time.Duration
	Version     string
	Now         time.Time
	// OnePerHub: collector.digest_mode one, with this collector at Rank in
	// Order and its turn at Slot.
	OnePerHub bool
	Order     []string
	Rank      int
	Slot      time.Time
}

// BuildDigest classifies every host and returns the digest, plus the state
// to save once it has been sent.
func BuildDigest(entries []Entry, prev DigestState, o DigestOptions) (*report.Digest, DigestState) {
	d := &report.Digest{
		Collector: o.Self, Version: o.Version, Generated: o.Now, SilentAfter: o.SilentAfter,
		Total: len(entries),
	}
	next := DigestState{Hosts: map[string]string{}}
	last := map[string]time.Time{}
	for _, e := range entries {
		last[e.Host] = e.ReceivedAt
		h := report.DigestHost{Host: e.Host, Last: e.ReceivedAt}
		was, seen := prev.Hosts[e.Host]
		if prev.Hosts != nil && !seen {
			d.NewHosts = append(d.NewHosts, e.Host)
		}
		wasOK := seen && (was == "ok" || was == "info")

		if o.Now.Sub(e.ReceivedAt) > o.SilentAfter {
			// Silent is never OK, whatever the last report said.
			h.Status, h.New = check.Unhealthy, wasOK
			d.Silent = append(d.Silent, h)
			next.Hosts[e.Host] = "silent"
			continue
		}
		h.Status = status(e)
		next.Hosts[e.Host] = h.Status.String()
		if !h.Status.Flagged() {
			d.OK++
			sumVitals(d, e.Vitals)
			continue
		}
		sumVitals(d, e.Vitals)
		h.New = wasOK
		alerts := e.Alerts
		if e.Err != nil {
			alerts = []check.Alert{{Status: check.Unhealthy, Msg: "report unreadable: " + e.Err.Error()}}
		}
		for i, a := range alerts {
			if i == 2 {
				h.More = len(alerts) - 2
				break
			}
			h.Alerts = append(h.Alerts, a.Msg)
		}
		d.NotOK = append(d.NotOK, h)
	}
	slices.SortStableFunc(d.NotOK, func(a, b report.DigestHost) int { return int(b.Status) - int(a.Status) })

	versions(d, entries)
	relay := map[string]string{}
	for _, e := range entries {
		if e.Err == nil {
			relay[e.Host] = e.RelayErr
		}
	}
	self := report.DigestPeer{Host: o.Self, Self: true, Last: last[o.Self], RelayErr: relay[o.Self]}
	for _, e := range entries {
		if e.Host == o.Self && e.Err == nil && newer(e.Version, o.Version) {
			self.Running, self.Installed = o.Version, e.Version
		}
	}
	d.Collectors = []report.DigestPeer{self}
	for _, p := range o.Peers {
		if p == o.Self {
			continue
		}
		t := last[p]
		d.Collectors = append(d.Collectors, report.DigestPeer{Host: p, Last: t, Silent: t.IsZero() || o.Now.Sub(t) > o.SilentAfter, RelayErr: relay[p]})
	}
	if o.OnePerHub {
		d.OnePerHub, d.Slot = true, o.Slot
		for i, c := range o.Order {
			switch {
			case i < o.Rank:
				d.Missed = append(d.Missed, c)
			case i > o.Rank:
				d.Standby = append(d.Standby, c)
			}
		}
	}

	d.Overall = check.OK
	for _, h := range d.NotOK {
		d.Overall = check.Worse(d.Overall, h.Status)
	}
	if len(d.Silent) > 0 {
		d.Overall = check.Unhealthy
	}
	for _, p := range d.Collectors {
		d.Overall = check.Worse(d.Overall, p.Status())
	}
	return d, next
}

// versions fills the digest's versions line: the hosts behind the newest lhc
// any host runs. A dev build or a report without a version counts as older.
func versions(d *report.Digest, entries []Entry) {
	var newest [3]int
	found := false
	label := ""
	for _, e := range entries {
		if v, ok := semver(e.Version); ok && e.Err == nil && (!found || slices.Compare(v[:], newest[:]) > 0) {
			newest, found, label = v, true, e.Version
		}
	}
	if !found {
		return
	}
	n := 0
	var older []string
	for _, e := range entries {
		if e.Err != nil {
			continue
		}
		if v, ok := semver(e.Version); ok && v == newest {
			n++
			continue
		}
		v := e.Version
		if v == "" {
			v = "unknown"
		}
		older = append(older, e.Host+" ("+v+")")
	}
	if len(older) == 0 {
		return
	}
	d.Newest, d.NewestHosts = strings.TrimPrefix(label, "v"), n
	if len(older) > report.OlderListed {
		d.OlderMore = len(older) - report.OlderListed
		older = older[:report.OlderListed]
	}
	d.Older = older
}

// newer says whether version a is later than b; false when either is not
// a release version (dev, empty).
func newer(a, b string) bool {
	va, okA := semver(a)
	vb, okB := semver(b)
	return okA && okB && slices.Compare(va[:], vb[:]) > 0
}

// semver reads a release version such as 0.9.1 or v0.9.1. A suffix (a
// local build's 0.9.1-3-gabcdef) is ignored.
func semver(v string) ([3]int, bool) {
	v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "-")
	v, _, _ = strings.Cut(v, "+")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// status is a host's overall status; a report that cannot be read counts
// as UNHEALTHY, never as OK.
func status(e Entry) check.Status {
	if e.Err != nil {
		return check.Unhealthy
	}
	st, ok := check.ParseStatus(e.Overall)
	if !ok {
		return check.Unhealthy
	}
	return st
}

// sumVitals adds one host's vital signs to the digest's totals.
func sumVitals(d *report.Digest, facts []check.Fact) {
	for _, f := range facts {
		switch f.Key {
		case "uptime":
			if up, ok := uptime(f.Value); ok {
				if d.UptimeMax == 0 || up < d.UptimeMin {
					d.UptimeMin = up
				}
				d.UptimeMax = max(d.UptimeMax, up)
			}
		case "updates":
			// "14", "14 (3 security)" or "unknown"
			if n, err := strconv.Atoi(strings.Fields(f.Value + " x")[0]); err == nil {
				d.Updates += n
			}
		case "reboot_required":
			d.Reboot++
		}
	}
}

// uptime reads the system check's "77 days", "5 hours", "1 minute".
func uptime(v string) (time.Duration, bool) {
	var n int
	var unit string
	if _, err := fmt.Sscanf(v, "%d %s", &n, &unit); err != nil {
		return 0, false
	}
	switch strings.TrimSuffix(unit, "s") {
	case "day":
		return time.Duration(n) * 24 * time.Hour, true
	case "hour":
		return time.Duration(n) * time.Hour, true
	case "minute":
		return time.Duration(n) * time.Minute, true
	}
	return 0, false
}

// Slot is the digest_time (hour, minute) most recently reached at now.
func Slot(now time.Time, hour, minute int) time.Time {
	s := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if s.After(now) {
		s = s.AddDate(0, 0, -1)
	}
	return s
}

// DigestDue says whether the scheduled digest should be sent now. The
// current slot is due until a digest for it was sent; after a refusal the
// next try waits RetryEvery. The following slot replaces a slot that never
// got through: yesterday's digest is no use today.
func DigestDue(now, slot, lastSent, lastTry time.Time) bool {
	if !lastSent.Before(slot) {
		return false
	}
	return lastTry.IsZero() || lastTry.Before(slot) || now.Sub(lastTry) >= RetryEvery
}

// ErrNoDigestYet is a store that has no host to summarise.
var ErrNoDigestYet = errors.New("no reports yet: nothing to summarise")
