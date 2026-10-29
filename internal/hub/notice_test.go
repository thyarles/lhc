package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
)

func TestEveryCollectorAgreesOnTheOrder(t *testing.T) {
	a, ra := Order("c2.example.com", []string{"C3.example.com", "c1.example.com"})
	b, rb := Order("c3.example.com", []string{"c1.example.com", "c2.example.com", "c3.example.com"})
	c, rc := Order("C1.example.com", []string{" c3.example.com", "c2.example.com"})
	want := []string{"c1.example.com", "c2.example.com", "c3.example.com"}
	if !slices.Equal(a, want) || !slices.Equal(b, want) || !slices.Equal(c, want) {
		t.Fatalf("%v %v %v", a, b, c)
	}
	if ra != 1 || rb != 2 || rc != 0 {
		t.Fatalf("ranks %d %d %d", ra, rb, rc)
	}
	if o, r := Order("solo.example.com", nil); len(o) != 1 || r != 0 {
		t.Fatal(o, r)
	}
}

func notice(t *testing.T, by, date string, at time.Time) *Envelope {
	t.Helper()
	b, _ := json.Marshal(Notice{Schema: 1, Date: date, By: by, At: at})
	return seal(t, key, by, at, b)
}

func (c *collector) postNotice(t *testing.T, e *Envelope) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(e)
	return c.raw(http.MethodPost, NoticePath, b)
}

func TestANoticeIsRecorded(t *testing.T) {
	c := newCollector(t)
	if w := c.postNotice(t, notice(t, "c1.example.com", "2026-01-05", t0)); w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, c.lastLog())
	}
	n, ok := Store{Dir: c.dir}.LoadNotice()
	if !ok || n.By != "c1.example.com" || n.Date != "2026-01-05" {
		t.Fatalf("%+v %v", n, ok)
	}
	if !strings.Contains(c.lastLog(), "digest for 2026-01-05 already sent by c1.example.com") {
		t.Fatal(c.lastLog())
	}
	// A notice is not a report: no host appears in the store.
	if l, _ := (Store{Dir: c.dir}).List(); len(l) != 0 {
		t.Fatal(l)
	}
}

func TestABadNoticeIsRefused(t *testing.T) {
	c := newCollector(t)
	wrongBy, _ := json.Marshal(Notice{Schema: 1, Date: "2026-01-05", By: "c9.example.com", At: t0})
	schema2, _ := json.Marshal(Notice{Schema: 2, Date: "2026-01-05", By: "c1.example.com", At: t0})
	badDate, _ := json.Marshal(Notice{Schema: 1, Date: "yesterday", By: "c1.example.com", At: t0})
	for name, e := range map[string]*Envelope{
		"wrong key":    seal(t, other, "c1.example.com", t0, wrongBy),
		"stale":        notice(t, "c1.example.com", "2026-01-05", t0.Add(-time.Hour)),
		"someone else": seal(t, key, "c1.example.com", t0, wrongBy),
		"schema":       seal(t, key, "c1.example.com", t0, schema2),
		"date":         seal(t, key, "c1.example.com", t0, badDate),
		"not a notice": seal(t, key, "c1.example.com", t0, []byte("[1,2]")),
	} {
		if w := c.postNotice(t, e); w.Code != http.StatusBadRequest || w.Body.Len() != 0 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if _, ok := (Store{Dir: c.dir}).LoadNotice(); ok {
		t.Fatal("a refused notice was recorded")
	}
	e := notice(t, "c1.example.com", "2026-01-05", t0)
	c.postNotice(t, e)
	if w := c.postNotice(t, e); w.Code != http.StatusBadRequest || !strings.Contains(c.lastLog(), "replayed") {
		t.Fatalf("replay: %d %s", w.Code, c.lastLog())
	}
}

// After a lost notice two collectors send; the day's sender stays the first.
func TestTheFirstNoticeOfTheDayStays(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for _, n := range []Notice{
		{Schema: 1, Date: "2026-01-05", By: "c1.example.com", At: t0},
		{Schema: 1, Date: "2026-01-05", By: "c2.example.com", At: t0.Add(15 * time.Minute)},
		{Schema: 1, Date: "2026-01-04", By: "c3.example.com", At: t0.Add(-24 * time.Hour)},
	} {
		if err := s.SaveNotice(n); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.LoadNotice(); n.By != "c1.example.com" {
		t.Fatal(n)
	}
	if err := s.SaveNotice(Notice{Schema: 1, Date: "2026-01-06", By: "c2.example.com", At: t0.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.LoadNotice(); n.By != "c2.example.com" || n.Date != "2026-01-06" {
		t.Fatal(n)
	}
}

func TestTellReachesEveryCollectorAndNamesAnOldOne(t *testing.T) {
	c := newCollector(t)
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	outs := Tell(context.Background(), key, []string{srv.URL, old.URL}, Notice{Schema: 1, Date: "2026-01-05", By: "c1.example.com", At: t0}, t0, time.Second)
	if Accepted(outs) != 1 || outs[0].Err != nil || !errors.Is(outs[1].Err, errOldPeer) {
		t.Fatalf("%+v", outs)
	}
}

func TestTheStoreReadsACollectorsRelayCheck(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	rep := func(host string, st check.Status, value string) []byte {
		b, _ := json.Marshal(map[string]any{
			"schema": 1, "host": host, "overall": "ok",
			"sections": []map[string]any{{"name": RelaySection, "rows": []check.Row{{Label: RelayRow, Value: value, Status: st}}}},
		})
		return b
	}
	_ = s.Put("c1.example.com", Stored{Report: rep("c1.example.com", check.OK, "reachable (relay.example.com:25)")})
	_ = s.Put("c2.example.com", Stored{Report: rep("c2.example.com", check.Caution, "connect relay.example.com:25: connection refused")})
	_ = s.Put("web01", Stored{Report: sample("web01")})
	l, _ := s.List()
	if !l[0].RelayChecked || l[0].RelayErr != "" || !l[1].RelayChecked || l[1].RelayErr != "connect relay.example.com:25: connection refused" || l[2].RelayChecked {
		t.Fatalf("%+v", l)
	}
}
