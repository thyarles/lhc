package hub

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/report"
)

func entry(host, overall string, ago time.Duration, alerts ...string) Entry {
	e := Entry{Host: host, Overall: overall, ReceivedAt: t0.Add(-ago)}
	for _, a := range alerts {
		e.Alerts = append(e.Alerts, check.Alert{Status: check.Caution, Msg: a})
	}
	return e
}

func digest(entries []Entry, prev DigestState, peers ...string) (*report.Digest, DigestState) {
	return BuildDigest(entries, prev, DigestOptions{
		Self: "c1.example.com", Peers: peers, SilentAfter: 26 * time.Hour, Version: "1.0.0", Now: t0,
	})
}

func names(hs []report.DigestHost) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Host)
	}
	return out
}

func TestTheDigestSortsHostsIntoOKNotOKAndSilent(t *testing.T) {
	d, next := digest([]Entry{
		entry("a.example.com", "ok", time.Hour),
		entry("b.example.com", "info", time.Hour),
		entry("c.example.com", "caution", time.Hour, "Disk /var at 91%"),
		entry("d.example.com", "unhealthy", time.Hour, "Service x failed"),
		entry("e.example.com", "ok", 30*time.Hour),
	}, DigestState{})
	if d.Total != 5 || d.OK != 2 {
		t.Fatalf("total %d, ok %d", d.Total, d.OK)
	}
	if got := names(d.NotOK); !slices.Equal(got, []string{"d.example.com", "c.example.com"}) {
		t.Fatalf("not OK, worst first: %v", got)
	}
	if got := names(d.Silent); !slices.Equal(got, []string{"e.example.com"}) {
		t.Fatalf("silent: %v", got)
	}
	if d.Overall != check.Unhealthy {
		t.Fatalf("overall %v", d.Overall)
	}
	if next.Hosts["e.example.com"] != "silent" || next.Hosts["c.example.com"] != "caution" {
		t.Fatalf("state %v", next.Hosts)
	}
	if got := report.DigestSubject(d); got != "[hub] 5 hosts · 2 OK · 1 CAUTION · 1 UNHEALTHY · 1 SILENT · via c1.example.com" {
		t.Fatalf("subject %q", got)
	}
}

func TestASilentHostIsNeverOKAtTheBoundary(t *testing.T) {
	d, _ := digest([]Entry{
		entry("on-time.example.com", "ok", 26*time.Hour),
		entry("late.example.com", "ok", 26*time.Hour+time.Second),
	}, DigestState{})
	if d.OK != 1 || !slices.Equal(names(d.Silent), []string{"late.example.com"}) {
		t.Fatalf("ok %d, silent %v", d.OK, names(d.Silent))
	}
}

func TestNewMeansOKInThePreviousDigest(t *testing.T) {
	prev := DigestState{Hosts: map[string]string{
		"was-ok.example.com": "ok", "was-bad.example.com": "caution", "was-info.example.com": "info",
	}}
	d, _ := digest([]Entry{
		entry("was-ok.example.com", "caution", time.Hour),
		entry("was-bad.example.com", "caution", time.Hour),
		entry("was-info.example.com", "ok", 40*time.Hour),
	}, prev)
	got := map[string]bool{}
	for _, h := range append(d.NotOK, d.Silent...) {
		got[h.Host] = h.New
	}
	if !got["was-ok.example.com"] || got["was-bad.example.com"] || !got["was-info.example.com"] {
		t.Fatalf("new markers %v", got)
	}
	if len(d.NewHosts) != 0 {
		t.Fatalf("new hosts %v", d.NewHosts)
	}
}

func TestNewHostsAreListedOnlyAfterAFirstDigest(t *testing.T) {
	entries := []Entry{entry("a.example.com", "ok", time.Hour), entry("b.example.com", "ok", time.Hour)}
	if d, _ := digest(entries, DigestState{}); len(d.NewHosts) != 0 {
		t.Fatalf("the first digest calls every host new: %v", d.NewHosts)
	}
	d, _ := digest(entries, DigestState{Hosts: map[string]string{"a.example.com": "ok"}})
	if !slices.Equal(d.NewHosts, []string{"b.example.com"}) {
		t.Fatalf("new hosts %v", d.NewHosts)
	}
}

func TestOnlyTheFirstTwoAlertsAreListed(t *testing.T) {
	d, _ := digest([]Entry{entry("a.example.com", "caution", time.Hour, "one", "two", "three", "four")}, DigestState{})
	if h := d.NotOK[0]; !slices.Equal(h.Alerts, []string{"one", "two"}) || h.More != 2 {
		t.Fatalf("%+v", h)
	}
}

func TestAnUnreadableReportIsUnhealthy(t *testing.T) {
	e := entry("a.example.com", "", time.Hour)
	e.Err = errors.New("unexpected end of JSON input")
	d, _ := digest([]Entry{e}, DigestState{})
	if d.OK != 0 || len(d.NotOK) != 1 || d.NotOK[0].Status != check.Unhealthy || d.NotOK[0].Alerts[0] != "report unreadable: unexpected end of JSON input" {
		t.Fatalf("%+v", d.NotOK)
	}
}

func TestASilentPeerIsNamedInTheSubject(t *testing.T) {
	d, _ := digest([]Entry{
		entry("c1.example.com", "ok", time.Hour),
		entry("c2.example.com", "ok", time.Hour),
		entry("c3.example.com", "ok", 30*time.Hour),
	}, DigestState{}, "c2.example.com", "c3.example.com", "c4.example.com")
	silent := map[string]bool{}
	for _, p := range d.Collectors {
		silent[p.Host] = p.Silent
	}
	if !d.Collectors[0].Self || silent["c1.example.com"] || silent["c2.example.com"] || !silent["c3.example.com"] || !silent["c4.example.com"] {
		t.Fatalf("%+v", d.Collectors)
	}
	want := "[hub] 3 hosts · 2 OK · 1 SILENT · via c1.example.com · peer c3.example.com SILENT · peer c4.example.com SILENT"
	if got := report.DigestSubject(d); got != want || d.Overall != check.Unhealthy {
		t.Fatalf("subject %q, overall %v", got, d.Overall)
	}
}

func TestVitalsAreSummedOverEveryHost(t *testing.T) {
	a := entry("a.example.com", "ok", time.Hour)
	a.Vitals = []check.Fact{{Key: "uptime", Value: "3 hours"}, {Key: "updates", Value: "14 (3 security)"}, {Key: "reboot_required", Value: "yes"}}
	b := entry("b.example.com", "caution", time.Hour)
	b.Vitals = []check.Fact{{Key: "uptime", Value: "1320 days"}, {Key: "updates", Value: "unknown"}}
	c := entry("c.example.com", "ok", time.Hour)
	c.Vitals = []check.Fact{{Key: "uptime", Value: "77 days"}, {Key: "updates", Value: "6"}}
	d, _ := digest([]Entry{a, b, c}, DigestState{})
	if got := d.Totals(); got != "uptime 3 hours to 1320 days · 20 pending updates · 1 host waiting for a reboot" {
		t.Fatalf("totals %q", got)
	}
}

func TestTheDigestStateRoundTrips(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if st := s.LoadDigest(); !st.Slot.IsZero() || st.Hosts != nil {
		t.Fatalf("no file: %+v", st)
	}
	want := DigestState{Slot: t0, Hosts: map[string]string{"a.example.com": "ok"}}
	if err := s.SaveDigest(want); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadDigest(); !got.Slot.Equal(t0) || got.Hosts["a.example.com"] != "ok" {
		t.Fatalf("%+v", got)
	}
	if hosts, err := s.List(); err != nil || len(hosts) != 0 {
		t.Fatalf("the state file is listed as a host: %v %v", hosts, err)
	}
}

func TestSlotIsTheLastDigestTimeReached(t *testing.T) {
	loc := time.FixedZone("BRT", -3*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 21, h, m, 0, 0, loc) }
	if got := Slot(at(8, 59), 9, 0); !got.Equal(at(9, 0).AddDate(0, 0, -1)) {
		t.Fatalf("08:59 → %v", got)
	}
	if got := Slot(at(9, 0), 9, 0); !got.Equal(at(9, 0)) {
		t.Fatalf("09:00 → %v", got)
	}
}

func TestTheDigestIsSentOncePerSlotAndRetriedEvery30Minutes(t *testing.T) {
	slot := t0
	yesterday := slot.AddDate(0, 0, -1)
	for _, c := range []struct {
		name            string
		now, sent, last time.Time
		want            bool
	}{
		{"due at the slot", slot, yesterday, time.Time{}, true},
		{"due after a restart", slot.Add(3 * time.Hour), yesterday, time.Time{}, true},
		{"never twice once sent", slot.Add(time.Minute), slot, slot, false},
		{"not again an hour later", slot.Add(time.Hour), slot, slot, false},
		{"refused: wait", slot.Add(29 * time.Minute), yesterday, slot, false},
		{"refused: retry after 30m", slot.Add(30 * time.Minute), yesterday, slot, true},
		{"a try for yesterday's slot does not delay today's", slot, yesterday, slot.Add(-10 * time.Minute), true},
	} {
		if got := DigestDue(c.now, slot, c.sent, c.last); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func versioned(host, version string) Entry {
	e := entry(host, "ok", time.Hour)
	e.Version = version
	return e
}

func TestOneVersionEverywhereNeedsNoVersionsLine(t *testing.T) {
	d, _ := digest([]Entry{versioned("a.example.com", "1.0.0"), versioned("b.example.com", "1.0.0")}, DigestState{})
	if d.VersionsLine() != "" {
		t.Fatal(d.VersionsLine())
	}
}

// After an upgrade loop, the hosts it skipped look like every other host.
func TestTheDigestNamesTheHostsBehindTheNewestVersion(t *testing.T) {
	var es []Entry
	for i := range 4 {
		es = append(es, versioned(fmt.Sprintf("new%d.example.com", i), "0.10.0"))
	}
	es = append(es,
		versioned("a.example.com", "0.9.1"),
		versioned("b.example.com", "dev"),
		versioned("c.example.com", ""),
		versioned("d.example.com", "0.9.10"),
		versioned("e.example.com", "0.8.1"),
		versioned("f.example.com", "0.8.1"),
	)
	broken := entry("g.example.com", "", time.Hour)
	broken.Err = errors.New("unexpected end of JSON input")
	es = append(es, broken)
	slices.SortFunc(es, func(a, b Entry) int { return strings.Compare(a.Host, b.Host) })

	d, _ := digest(es, DigestState{})
	want := "lhc 0.10.0 on 4 hosts · older: a.example.com (0.9.1), b.example.com (dev), c.example.com (unknown), d.example.com (0.9.10), e.example.com (0.8.1) (+1 more)"
	if got := d.VersionsLine(); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestNoReleaseVersionAnywhereNeedsNoVersionsLine(t *testing.T) {
	d, _ := digest([]Entry{versioned("a.example.com", "dev"), versioned("b.example.com", "")}, DigestState{})
	if d.VersionsLine() != "" {
		t.Fatal(d.VersionsLine())
	}
}

// Upgraded binary, old process: the collectors kept running a release with
// no digest code, and nothing on the hub said so (#36).
func TestACollectorRunningAnOlderProcessThanItsBinarySaysSo(t *testing.T) {
	d, _ := digest([]Entry{versioned("c1.example.com", "1.1.0")}, DigestState{})
	self := d.Collectors[0]
	if self.Status() != check.Caution || d.PeerText(self) != "this one · running lhc 1.0.0, installed 1.1.0: restart lhc-collect" {
		t.Fatalf("%v %q", self.Status(), d.PeerText(self))
	}
	if d.Overall != check.Caution {
		t.Fatalf("overall %v", d.Overall)
	}
	for _, v := range []string{"1.0.0", "0.9.1", "dev", ""} {
		d, _ := digest([]Entry{versioned("c1.example.com", v)}, DigestState{})
		if p := d.Collectors[0]; p.Status() != check.OK || d.PeerText(p) != "this one" {
			t.Fatalf("own report %q: %v %q", v, p.Status(), d.PeerText(p))
		}
	}
}

func TestSemver(t *testing.T) {
	for v, want := range map[string]bool{
		"0.9.1": true, "v1.2.3": true, "0.9.1-3-gabcdef": true, "0.9.1-dirty": true,
		"dev": false, "": false, "1.2": false, "1.2.x": false, "1.2.3.4": false,
	} {
		if _, ok := semver(v); ok != want {
			t.Errorf("semver(%q) ok = %v", v, ok)
		}
	}
	if !newer("0.10.0", "0.9.9") || newer("0.9.1", "0.9.1") || newer("1.0.0", "dev") {
		t.Fatal("newer")
	}
}

func TestTheDigestFlagsACollectorThatCannotReachTheRelay(t *testing.T) {
	c2 := entry("c2.example.com", "caution", time.Hour, RelayAlert)
	c2.RelayChecked, c2.RelayErr = true, "connect relay.example.com:25: connection refused"
	old := entry("c3.example.com", "ok", time.Hour) // older lhc: no relay check
	d, _ := digest([]Entry{entry("c1.example.com", "ok", time.Hour), c2, old}, DigestState{}, "c2.example.com", "c3.example.com")
	p := d.Collectors[1]
	if p.Status() != check.Caution || d.PeerText(p) != "last report 2026-01-05 06:00 (1h ago) · relay unreachable: connect relay.example.com:25: connection refused" {
		t.Fatalf("%v %q", p.Status(), d.PeerText(p))
	}
	if q := d.Collectors[2]; q.Status() != check.OK || strings.Contains(d.PeerText(q), "relay") {
		t.Fatalf("an older peer was marked: %q", d.PeerText(q))
	}
	if d.Overall != check.Caution {
		t.Fatal(d.Overall)
	}
}

func TestTheDigestSaysWhoSentItAndWhoStandsBy(t *testing.T) {
	order := []string{"c1.example.com", "c2.example.com", "c3.example.com"}
	slot := t0.Add(-time.Hour)
	for rank, want := range []string{
		"Sent by c1.example.com · standby c2.example.com, c3.example.com",
		"Sent by c2.example.com: no digest from c1.example.com by " + slot.Format("15:04"),
		"Sent by c3.example.com: no digest from c1.example.com, c2.example.com by " + slot.Format("15:04"),
	} {
		d, _ := BuildDigest([]Entry{entry("a.example.com", "ok", time.Hour)}, DigestState{}, DigestOptions{
			Self: order[rank], SilentAfter: 26 * time.Hour, Version: "1.0.0", Now: t0,
			OnePerHub: true, Order: order, Rank: rank, Slot: slot,
		})
		if got := d.SenderLine(); got != want {
			t.Errorf("rank %d: %q, want %q", rank, got, want)
		}
	}
	if d, _ := digest([]Entry{entry("a.example.com", "ok", time.Hour)}, DigestState{}); d.SenderLine() != "" {
		t.Fatal(d.SenderLine())
	}
}
