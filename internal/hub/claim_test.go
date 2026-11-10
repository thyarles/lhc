package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/report"
)

const silent = 26 * time.Hour

// member stores a report from host in group, received at at.
func member(t *testing.T, s Store, host, group string, at time.Time, cr *report.CertRestart) {
	t.Helper()
	if cr == nil {
		cr = &report.CertRestart{Group: group, BeforeDays: 30}
	}
	b, _ := json.Marshal(map[string]any{
		"schema": 1, "host": host, "overall": "ok", "generated_at": at, "sections": []any{}, "cert_restart": cr,
	})
	if err := s.Put(host, Stored{ReceivedAt: at, Report: b}); err != nil {
		t.Fatal(err)
	}
}

func claimFor(host, group string) Claim {
	return Claim{Schema: 1, Kind: KindCertRestart, Host: host, Group: group, ID: "id-" + host}
}

// claimServer is a collector named self behind a real HTTP server.
func claimServer(t *testing.T, self string) (*collector, string) {
	t.Helper()
	c := newCollector(t)
	c.ConfigureClaims(self, silent)
	srv := httptest.NewServer(c.Receiver)
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func askAll(urls []string, c Claim, at time.Time) []Reply {
	return Ask(context.Background(), key, urls, c, func() time.Time { return at }, 2*time.Second)
}

func TestOneHostOfAGroupADay(t *testing.T) {
	var urls []string
	for i := 1; i <= 3; i++ {
		_, u := claimServer(t, fmt.Sprintf("c%d.example.com", i))
		urls = append(urls, u)
	}
	a := askAll(urls, claimFor("k8s-node01.example.com", "k8s-node"), t0)
	if Grants(a) != 3 {
		t.Fatalf("first claim: %+v", a)
	}
	b := askAll(urls, claimFor("k8s-node02.example.com", "k8s-node"), t0.Add(time.Minute))
	if Grants(b) != 0 || !strings.Contains(b[0].Text(), "refused: k8s-node01.example.com restarts today") {
		t.Fatalf("second claim: %q", b[0].Text())
	}
	if b[0].Answer.By != "c1.example.com" {
		t.Fatalf("answer by %q", b[0].Answer.By)
	}
	other := askAll(urls, claimFor("db01.example.com", "db"), t0.Add(2*time.Minute))
	if Grants(other) != 3 {
		t.Fatalf("another group the same night: %+v", other)
	}
	again := askAll(urls, claimFor("k8s-node01.example.com", "k8s-node"), t0.Add(3*time.Minute))
	if Grants(again) != 3 || !strings.Contains(again[0].Answer.Reason, "already granted") {
		t.Fatalf("asking again after a lost answer: %+v", again)
	}
}

func TestExactlyOneMajorityWithACollectorDown(t *testing.T) {
	_, u1 := claimServer(t, "c1.example.com")
	_, u2 := claimServer(t, "c2.example.com")
	down := httptest.NewServer(http.NotFoundHandler())
	u3 := down.URL
	down.Close() // nothing listens there now
	all := []string{u1, u2, u3}

	// Whatever the order, and whichever collector each host reaches.
	a := askAll(all, claimFor("k8s-node01.example.com", "k8s-node"), t0)
	b := askAll(all, claimFor("k8s-node02.example.com", "k8s-node"), t0)
	if Grants(a) != 2 || Grants(b) != 0 || a[2].Err == nil {
		t.Fatalf("a %d b %d, down: %v", Grants(a), Grants(b), a[2].Err)
	}
	if Majority(3) != 2 || Majority(2) != 2 || Majority(1) != 1 {
		t.Fatal("majority")
	}
}

func TestASplitNetworkGivesNoSecondMajority(t *testing.T) {
	_, u1 := claimServer(t, "c1.example.com")
	_, u2 := claimServer(t, "c2.example.com")
	_, u3 := claimServer(t, "c3.example.com")
	// node01 reaches c1 only; node02 reaches c2 and c3: one grant each way
	// at most, so only node02 has a majority of three.
	a := askAll([]string{u1}, claimFor("k8s-node01.example.com", "k8s-node"), t0)
	b := askAll([]string{u2, u3}, claimFor("k8s-node02.example.com", "k8s-node"), t0)
	if Grants(a) >= Majority(3) || Grants(b) < Majority(3) {
		t.Fatalf("a %d b %d", Grants(a), Grants(b))
	}
	// node01 then reaches everyone: c2 and c3 granted node02 today.
	if a2 := askAll([]string{u1, u2, u3}, claimFor("k8s-node01.example.com", "k8s-node"), t0.Add(time.Minute)); Grants(a2) >= Majority(3) {
		t.Fatalf("second majority: %+v", a2)
	}
}

func TestGrantRules(t *testing.T) {
	day1 := time.Date(2026, 1, 5, 23, 0, 0, 0, time.UTC)
	node1, node2, node3 := "k8s-node01.example.com", "k8s-node02.example.com", "k8s-node03.example.com"

	t.Run("20h between grants, across midnight", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		if ok, _, _ := s.Decide(claimFor(node1, "k8s-node"), day1, silent); !ok {
			t.Fatal("first")
		}
		member(t, s, node1, "k8s-node", day1.Add(30*time.Minute), nil)
		ok, why, _ := s.Decide(claimFor(node2, "k8s-node"), day1.Add(2*time.Hour), silent)
		if ok || !strings.Contains(why, "less than 20h") {
			t.Fatalf("%v %q", ok, why)
		}
		member(t, s, node1, "k8s-node", day1.Add(20*time.Hour), nil)
		if ok, why, _ := s.Decide(claimFor(node2, "k8s-node"), day1.Add(21*time.Hour), silent); !ok {
			t.Fatalf("next night: %q", why)
		}
	})
	t.Run("a granted host that never reported again blocks the group", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		member(t, s, node1, "k8s-node", day1.Add(-time.Hour), nil)
		_, _, _ = s.Decide(claimFor(node1, "k8s-node"), day1, silent)
		ok, why, _ := s.Decide(claimFor(node2, "k8s-node"), day1.Add(25*time.Hour), silent)
		if ok || !strings.Contains(why, node1+" has not reported since its restart") {
			t.Fatalf("%v %q", ok, why)
		}
	})
	t.Run("a SILENT member blocks the group", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		member(t, s, node3, "k8s-node", day1.Add(-30*time.Hour), nil)
		member(t, s, "db01.example.com", "db", day1.Add(-30*time.Hour), nil)
		ok, why, _ := s.Decide(claimFor(node1, "k8s-node"), day1, silent)
		if ok || why != node3+" is SILENT (no report for over 26h)" {
			t.Fatalf("%v %q", ok, why)
		}
		// A silent host of another group does not matter.
		member(t, s, node3, "k8s-node", day1, nil)
		if ok, why, _ := s.Decide(claimFor(node1, "k8s-node"), day1, silent); !ok {
			t.Fatal(why)
		}
	})
	t.Run("a probe records nothing", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		p := claimFor(node1, "k8s-node")
		p.Probe = true
		if ok, why, _ := s.Decide(p, day1, silent); !ok || why != "would be granted" {
			t.Fatalf("%v %q", ok, why)
		}
		if g := s.LoadGrants(); len(g) != 0 {
			t.Fatalf("probe recorded %+v", g)
		}
	})
	t.Run("grants older than 60 days are forgotten", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		_, _, _ = s.Decide(claimFor(node1, "k8s-node"), day1, silent)
		_, _, _ = s.Decide(claimFor(node2, "k8s-node"), day1.Add(61*24*time.Hour), silent)
		g := s.LoadGrants()
		if len(g) != 1 || g[0].Host != node2 {
			t.Fatalf("%+v", g)
		}
	})
}

func TestABadClaimIsRefusedWithAnEmpty400(t *testing.T) {
	c := newCollector(t)
	c.ConfigureClaims("c1.example.com", silent)
	body := func(cl Claim) []byte { b, _ := json.Marshal(cl); return b }
	good := claimFor("k8s-node01.example.com", "k8s-node")
	wrongHost, badKind, badGroup, noID := good, good, good, good
	wrongHost.Host = "k8s-node09.example.com"
	badKind.Kind = "reboot"
	badGroup.Group = "../etc"
	noID.ID = ""
	for name, e := range map[string]*Envelope{
		"wrong key":   seal(t, other, good.Host, t0, body(good)),
		"stale":       seal(t, key, good.Host, t0.Add(-time.Hour), body(good)),
		"other host":  seal(t, key, good.Host, t0, body(wrongHost)),
		"unknown":     seal(t, key, good.Host, t0, body(badKind)),
		"bad group":   seal(t, key, good.Host, t0, body(badGroup)),
		"no ID":       seal(t, key, good.Host, t0, body(noID)),
		"not a claim": seal(t, key, good.Host, t0, []byte("[1]")),
	} {
		b, _ := json.Marshal(e)
		if w := c.raw(http.MethodPost, ClaimPath, b); w.Code != http.StatusBadRequest || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", name, w.Code, w.Body.String())
		}
	}
	if g := (Store{Dir: c.dir}).LoadGrants(); len(g) != 0 {
		t.Fatalf("granted %+v", g)
	}

	// The same genuine envelope twice: the replay is refused.
	e, _ := json.Marshal(seal(t, key, good.Host, t0, body(good)))
	if w := c.raw(http.MethodPost, ClaimPath, e); w.Code != http.StatusOK {
		t.Fatalf("first: %d %s", w.Code, c.lastLog())
	}
	if w := c.raw(http.MethodPost, ClaimPath, e); w.Code != http.StatusBadRequest || !strings.Contains(c.lastLog(), "replayed") {
		t.Fatalf("replay: %d %s", w.Code, c.lastLog())
	}

	// A collector that does not know its own name grants nothing.
	nameless := newCollector(t)
	e2, _ := json.Marshal(seal(t, key, good.Host, t0, body(good)))
	if w := nameless.raw(http.MethodPost, ClaimPath, e2); w.Code != http.StatusBadRequest {
		t.Fatalf("nameless: %d", w.Code)
	}
}

func TestTheHostTrustsOnlyASealedMatchingAnswer(t *testing.T) {
	cl := claimFor("k8s-node01.example.com", "k8s-node")
	answer := func(k []byte, a Answer, at time.Time) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, _ := json.Marshal(a)
			e, _ := Seal(k, a.By, at, b)
			out, _ := json.Marshal(e)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
		}
	}
	granted := Answer{Schema: 1, ID: cl.ID, By: "c1.example.com", Granted: true, Reason: "granted"}
	otherID := granted
	otherID.ID = "id-someone-else"
	cases := map[string]struct {
		h    http.Handler
		want string
	}{
		"genuine":      {answer(key, granted, t0), ""},
		"wrong key":    {answer(other, granted, t0), "does not decrypt"},
		"replayed":     {answer(key, otherID, t0), "does not match the claim"},
		"stale":        {answer(key, granted, t0.Add(-time.Hour)), "from this host's clock"},
		"older lhc":    {http.NotFoundHandler(), "older lhc"},
		"refused":      {http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }), "wrong hub key"},
		"not lhc":      {http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }), "is this an lhc collector"},
		"bare 200 OK":  {http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), "not an envelope"},
		"plain grant!": {http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"granted":true}`)) }), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			r := askAll([]string{srv.URL}, cl, t0)[0]
			if name == "genuine" {
				if !r.Granted() {
					t.Fatalf("%+v", r)
				}
				return
			}
			if r.Granted() {
				t.Fatalf("acted on %+v", r)
			}
			if tc.want != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.want)) {
				t.Fatalf("err %v, want %q", r.Err, tc.want)
			}
			if r.Err != nil && errors.Is(r.Err, errNoClaims) != (name == "older lhc") {
				t.Fatalf("errNoClaims mismatch: %v", r.Err)
			}
		})
	}
}

func TestDigestCertificateRestartLines(t *testing.T) {
	now := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	restarted := now.Add(-5 * time.Hour)
	entries := []Entry{
		{Host: "k8s-node01.example.com", ReceivedAt: now.Add(-4 * time.Hour), CertRestart: &report.CertRestart{
			Group: "k8s-node", BeforeDays: 30, FirstExpiry: now.Add(364 * 24 * time.Hour), Service: "rke2-server", LastRestart: restarted, LastRenewed: true,
		}},
		{Host: "k8s-node02.example.com", ReceivedAt: now.Add(-time.Hour), CertRestart: &report.CertRestart{
			Group: "k8s-node", BeforeDays: 30, FirstExpiry: now.Add(29*24*time.Hour + time.Hour),
		}},
		{Host: "k8s-node03.example.com", ReceivedAt: now.Add(-20 * time.Hour), CertRestart: &report.CertRestart{
			Group: "k8s-node", BeforeDays: 30, FirstExpiry: now.Add(20*24*time.Hour + time.Hour),
		}},
		{Host: "k8s-worker01.example.com", ReceivedAt: now.Add(-time.Hour), CertRestart: &report.CertRestart{
			Group: "k8s-worker", BeforeDays: 30, FirstExpiry: now.Add(25*24*time.Hour + time.Hour), Service: "rke2-agent", LastRestart: now.Add(-2 * time.Hour),
		}},
		{Host: "db01.example.com", ReceivedAt: now.Add(-time.Hour)},
	}
	grants := []Grant{
		{Kind: KindCertRestart, Group: "k8s-node", Host: "k8s-node01.example.com", At: restarted},
		{Kind: KindCertRestart, Group: "k8s-node", Host: "k8s-node03.example.com", At: now.Add(-6 * time.Hour)},
	}
	gs := certGroups(entries, grants, now)
	if len(gs) != 2 || gs[0].Name != "k8s-node" || gs[0].Hosts != 3 || gs[1].Name != "k8s-worker" {
		t.Fatalf("%+v", gs)
	}
	var texts []string
	for _, l := range gs[0].Lines {
		texts = append(texts, fmt.Sprintf("%s|%s", l.Status.Word(), l.Text))
	}
	want := []string{
		"OK|k8s-node01.example.com restarted rke2-server at 04:00, renewed (first expires in 364d)",
		"UNHEALTHY|k8s-node03.example.com was granted a restart at 2026-01-06 03:00, no report since",
		"INFO|due: k8s-node03.example.com (expires in 20d), k8s-node02.example.com (expires in 29d)",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	if gs[0].Status() != check.Unhealthy {
		t.Fatal(gs[0].Status())
	}
	if l := gs[1].Lines[0]; l.Status != check.Caution || !strings.Contains(l.Text, "restart did not renew (first still expires in 25d)") {
		t.Fatalf("%+v", l)
	}
	if certGroups(entries[4:], nil, now) != nil {
		t.Fatal("no cert_restart host: no block")
	}

	d, _ := BuildDigest(entries, DigestState{}, DigestOptions{Self: "c1.example.com", SilentAfter: silent, Now: now, Grants: grants})
	if len(d.CertGroups) != 2 || d.Overall != check.Unhealthy {
		t.Fatalf("overall %v groups %d", d.Overall, len(d.CertGroups))
	}
}
