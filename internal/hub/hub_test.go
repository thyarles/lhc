package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	key   = bytes.Repeat([]byte{7}, KeySize)
	other = bytes.Repeat([]byte{8}, KeySize)
	t0    = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)
)

func report(host string) []byte {
	return []byte(`{"schema":1,"host":"` + host + `","overall":"caution","generated_at":"2026-01-05T07:00:00Z","sections":[]}`)
}

func seal(t *testing.T, k []byte, host string, at time.Time, rep []byte) *Envelope {
	t.Helper()
	e, err := Seal(k, host, at, rep)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestKeygenAndLoad(t *testing.T) {
	b, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey(b)
	if err != nil || len(k) != KeySize {
		t.Fatalf("%v %d", err, len(k))
	}
	p := filepath.Join(t.TempDir(), "hub.key")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable key was accepted: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(p); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "not base64!", "c2hvcnQ="} {
		if _, err := ParseKey([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	e := seal(t, key, "web01.example.com", t0, report("web01.example.com"))
	got, err := Open(key, e, 1<<20)
	if err != nil || !bytes.Equal(got, report("web01.example.com")) {
		t.Fatalf("%v %s", err, got)
	}
	if _, err := Open(other, e, 1<<20); err == nil {
		t.Fatal("opened with the wrong key")
	}
}

// The header is authenticated with the data, so none of it can be swapped.
func TestTamperingIsDetected(t *testing.T) {
	for name, tamper := range map[string]func(e *Envelope){
		"host":    func(e *Envelope) { e.Host = "web02.example.com" },
		"sent_at": func(e *Envelope) { e.SentAt = t0.Add(time.Minute).Format(time.RFC3339) },
		"data":    func(e *Envelope) { e.Data[3] ^= 1 },
		"nonce":   func(e *Envelope) { e.Nonce[0] ^= 1 },
		"version": func(e *Envelope) { e.V = 2 },
	} {
		e := seal(t, key, "web01.example.com", t0, report("web01.example.com"))
		tamper(e)
		if _, err := Open(key, e, 1<<20); err == nil {
			t.Errorf("tampered %s was accepted", name)
		}
	}
}

func TestHostNames(t *testing.T) {
	for _, ok := range []string{"web01", "web01.example.com", "db_2-a"} {
		if !ValidHost(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "../etc/passwd", "a/b", ".hidden", "..", "web 01", "web01\n", strings.Repeat("a", 254)} {
		if ValidHost(bad) {
			t.Errorf("accepted %q", bad)
		}
		if _, err := Seal(key, bad, t0, report(bad)); err == nil {
			t.Errorf("sealed %q", bad)
		}
	}
}

type collector struct {
	*Receiver
	dir string
	mu  sync.Mutex
	now time.Time
	log []string
}

func newCollector(t *testing.T) *collector {
	c := &collector{dir: t.TempDir(), now: t0}
	c.Receiver = NewReceiver(key, 1<<20, Store{Dir: c.dir}, func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.now
	}, func(f string, a ...any) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.log = append(c.log, fmt.Sprintf(f, a...))
	})
	return c
}

func (c *collector) post(t *testing.T, e *Envelope) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(e)
	return c.raw(http.MethodPost, Path, b)
}

func (c *collector) raw(method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.5:40000"
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)
	return w
}

func (c *collector) lastLog() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.log) == 0 {
		return ""
	}
	return c.log[len(c.log)-1]
}

func TestAnAcceptedReportIsStored(t *testing.T) {
	c := newCollector(t)
	w := c.post(t, seal(t, key, "web01.example.com", t0, report("web01.example.com")))
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	entries, err := c.Store.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %+v", err, entries)
	}
	e := entries[0]
	if e.Host != "web01.example.com" || e.Overall != "caution" || !e.ReceivedAt.Equal(t0) || e.Err != nil {
		t.Fatalf("%+v", e)
	}
	b, _ := os.ReadFile(filepath.Join(c.dir, "web01.example.com.json"))
	var st Stored
	if err := json.Unmarshal(b, &st); err != nil || st.RemoteAddr != "192.0.2.5" {
		t.Fatalf("%v %+v", err, st)
	}
	if fi, _ := os.Stat(filepath.Join(c.dir, "web01.example.com.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(c.lastLog(), "accepted web01.example.com from 192.0.2.5") {
		t.Fatal(c.lastLog())
	}
}

// Every refusal looks the same from outside; only the log says why.
func refused(t *testing.T, c *collector, w *httptest.ResponseRecorder, why string) {
	t.Helper()
	if w.Code != http.StatusBadRequest || w.Body.Len() != 0 {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	if !strings.Contains(c.lastLog(), why) {
		t.Fatalf("log %q does not say %q", c.lastLog(), why)
	}
}

func nothingStored(t *testing.T, c *collector) {
	t.Helper()
	if entries, _ := c.Store.List(); len(entries) > 0 {
		t.Fatalf("stored a refused report: %+v", entries)
	}
}

func TestRefusals(t *testing.T) {
	traversal := seal(t, key, "web01", t0, report("web01"))
	traversal.Host = "../etc/passwd"
	envelope := func(e *Envelope) []byte { b, _ := json.Marshal(e); return b }
	for _, tc := range []struct {
		name, why string
		body      []byte
	}{
		{"wrong key", "does not decrypt", envelope(seal(t, other, "web01", t0, report("web01")))},
		{"clock behind", "from this collector's clock", envelope(seal(t, key, "web01", t0.Add(-11*time.Minute), report("web01")))},
		{"clock ahead", "from this collector's clock", envelope(seal(t, key, "web01", t0.Add(11*time.Minute), report("web01")))},
		{"not schema 1", "schema 2", envelope(seal(t, key, "web01", t0, []byte(`{"schema":2,"host":"web01"}`)))},
		{"report for another host", `inside is for "web02"`, envelope(seal(t, key, "web01", t0, report("web02")))},
		{"path in the host name", "bad host name", envelope(traversal)},
		{"not json", "not an envelope", []byte("hello")},
		{"unknown field", "not an envelope", []byte(`{"v":1,"cmd":"rm"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCollector(t)
			refused(t, c, c.raw(http.MethodPost, Path, tc.body), tc.why)
			nothingStored(t, c)
		})
	}
}

func TestSkewInsideTheWindowIsAccepted(t *testing.T) {
	c := newCollector(t)
	for i, d := range []time.Duration{-9 * time.Minute, 9 * time.Minute} {
		if w := c.post(t, seal(t, key, fmt.Sprintf("web0%d", i), t0.Add(d), report(fmt.Sprintf("web0%d", i)))); w.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", d, w.Code, c.lastLog())
		}
	}
}

func TestAReplayIsRefusedForTheWholeWindow(t *testing.T) {
	c := newCollector(t)
	// Sent from a clock 9 minutes ahead: the envelope stays fresh until 19
	// minutes after the collector first accepted it.
	e := seal(t, key, "web01", t0.Add(9*time.Minute), report("web01"))
	if w := c.post(t, e); w.Code != http.StatusNoContent {
		t.Fatal(c.lastLog())
	}
	refused(t, c, c.post(t, e), "replayed")
	// Pruning has run by then, and the nonce must have survived it.
	c.now = t0.Add(19 * time.Minute)
	refused(t, c, c.post(t, e), "replayed")
	c.now = t0.Add(20 * time.Minute)
	refused(t, c, c.post(t, e), "from this collector's clock")
}

func TestOnlyThePostRouteAnswers(t *testing.T) {
	c := newCollector(t)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, Path},
		{http.MethodPut, Path},
		{http.MethodPost, "/v1/report/"},
		{http.MethodPost, "/v2/report"},
		{http.MethodGet, "/debug/pprof/"},
		{http.MethodPost, "/"},
	} {
		w := c.raw(r.method, r.path, nil)
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Errorf("%s %s: %d %q", r.method, r.path, w.Code, w.Body)
		}
	}
}

func TestTheRequestSizeIsLimited(t *testing.T) {
	c := newCollector(t)
	c.Configure(key, 1024)
	refused(t, c, c.raw(http.MethodPost, Path, bytes.Repeat([]byte(" "), 2048)), "larger than collector.max_report_size")
}

// A small request that inflates to gigabytes must stop at the limit.
func TestAGzipBombStopsAtTheLimit(t *testing.T) {
	c := newCollector(t)
	c.Configure(key, 64<<10)
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	chunk := make([]byte, 1<<20)
	for range 8 { // 8 MiB of zeros, about 8 KB compressed
		_, _ = zw.Write(chunk)
	}
	_ = zw.Close()
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	e := &Envelope{V: Version, Host: "web01", SentAt: t0.Format(time.RFC3339), Nonce: make([]byte, 12)}
	e.Data = aead.Seal(nil, e.Nonce, z.Bytes(), e.aad())
	refused(t, c, c.post(t, e), "once decompressed")
}

func TestTheRateIsLimitedPerAddress(t *testing.T) {
	c := newCollector(t)
	for range RatePerMinute {
		c.raw(http.MethodPost, Path, []byte("x"))
	}
	refused(t, c, c.post(t, seal(t, key, "web01", t0, report("web01"))), "requests in a minute")
	c.now = t0.Add(time.Minute)
	if w := c.post(t, seal(t, key, "web01", c.now, report("web01"))); w.Code != http.StatusNoContent {
		t.Fatal(c.lastLog())
	}
}

func TestForget(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Put("web01", Stored{Report: report("web01")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("../web01"); err == nil {
		t.Fatal("forgot a path")
	}
	if err := s.Forget("web01"); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("web01"); err == nil {
		t.Fatal("forgot a host twice")
	}
	if l, _ := s.List(); len(l) != 0 {
		t.Fatal(l)
	}
}

func TestSendToEveryCollectorEvenWithOneDown(t *testing.T) {
	var got []string
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path+" "+r.Header.Get("Content-Type"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	down := "http://" + l.Addr().String()
	_ = l.Close()

	e := seal(t, key, "web01", t0, report("web01"))
	outs := Send(context.Background(), []string{down, up.URL + "/"}, e, time.Second)
	if len(outs) != 2 || outs[0].Err == nil || outs[1].Err != nil || Accepted(outs) != 1 {
		t.Fatalf("%+v", outs)
	}
	if len(got) != 1 || got[0] != "/v1/report application/json" {
		t.Fatal(got)
	}
}

func TestSendTimesOut(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)
	start := time.Now()
	outs := Send(context.Background(), []string{slow.URL}, seal(t, key, "web01", t0, report("web01")), 200*time.Millisecond)
	if outs[0].Err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v after %s", outs, time.Since(start))
	}
}

func TestARefusalExplainsItself(t *testing.T) {
	no := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	defer no.Close()
	outs := Send(context.Background(), []string{no.URL}, seal(t, key, "web01", t0, report("web01")), time.Second)
	if outs[0].Err == nil || !strings.Contains(outs[0].Err.Error(), "wrong hub key") {
		t.Fatalf("%+v", outs)
	}
}
