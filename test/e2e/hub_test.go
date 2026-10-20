//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a log the test reads while the collector still writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type collector struct {
	*host
	url string
	log *syncBuffer
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// keygen writes a hub key with the binary itself.
func keygen(t *testing.T, h *host) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hub.key")
	h.run(0, "hub", "keygen", "--out", p)
	return p
}

// startCollector makes h a collector and runs `lhc collect` until the test ends.
func startCollector(t *testing.T, h *host, key string) *collector {
	t.Helper()
	addr := freeAddr(t)
	h.run(0, "config", "set", "collector.enabled=true", "collector.listen="+addr, "hub.key_file="+key)
	c := &collector{host: h, url: "http://" + addr, log: &syncBuffer{}}
	cmd := exec.Command(binary, "collect", "--config", h.cfg)
	cmd.Stdout, cmd.Stderr = c.log, c.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("lhc collect did not stop cleanly on SIGTERM: %v\n%s", err, c.log)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("lhc collect ignored SIGTERM")
		}
	})
	for i := 0; ; i++ {
		if conn, err := net.Dial("tcp", addr); err == nil {
			_ = conn.Close()
			return c
		}
		if i == 100 {
			t.Fatalf("lhc collect never listened:\n%s", c.log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stored is the report a collector keeps for one host, or nil.
func (c *collector) stored(t *testing.T, host string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.state, "hub", host+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st.Report
}

// hubHost is a host that sends to the given collectors.
func hubHost(t *testing.T, name, key string, collectors ...string) *host {
	t.Helper()
	h := newHost(t, relay(t))
	h.run(0, "config", "set", "hostname="+name, "hub.key_file="+key, "hub.collectors="+strings.Join(collectors, ","))
	return h
}

func TestEveryCollectorStoresEveryHost(t *testing.T) {
	c1 := newHost(t, relay(t))
	key := keygen(t, c1)
	col1 := startCollector(t, c1, key)
	col2 := startCollector(t, newHost(t, relay(t)), key)

	for _, name := range []string{"a.example.com", "b.example.com"} {
		h := hubHost(t, name, key, col1.url, col2.url)
		h.run(0, "run")
		if !strings.Contains(h.stderr, "Hub: 2 of 2 collector(s) accepted the report") {
			t.Fatalf("%s:\n%s", name, h.stderr)
		}
		if len(h.relay.Messages()) != 1 {
			t.Fatal("the mail must still go out")
		}
	}
	for _, c := range []*collector{col1, col2} {
		for _, name := range []string{"a.example.com", "b.example.com"} {
			rep := c.stored(t, name)
			if rep == nil || rep["host"] != name || rep["schema"] != float64(1) || rep["overall"] != "unhealthy" {
				t.Fatalf("%s on %s: %v\n%s", name, c.url, rep, c.log)
			}
			if !strings.Contains(c.log.String(), "accepted "+name+" from 127.0.0.1 (unhealthy)") {
				t.Fatalf("not logged:\n%s", c.log)
			}
		}
	}
	col1.run(0, "hub", "status")
	for _, want := range []string{"HOST", "a.example.com", "b.example.com", "unhealthy", "just now"} {
		if !strings.Contains(col1.stdout, want) {
			t.Fatalf("hub status lacks %q:\n%s", want, col1.stdout)
		}
	}
	col1.run(0, "hub", "forget", "a.example.com")
	col1.run(0, "hub", "status")
	if strings.Contains(col1.stdout, "a.example.com") {
		t.Fatalf("forgotten host still listed:\n%s", col1.stdout)
	}
}

func TestAWrongKeyIsRefused(t *testing.T) {
	c := newHost(t, relay(t))
	col := startCollector(t, c, keygen(t, c))
	h := hubHost(t, "a.example.com", keygen(t, c), col.url)
	h.run(0, "run")
	if col.stored(t, "a.example.com") != nil {
		t.Fatal("stored a report sealed with another key")
	}
	if !strings.Contains(col.log.String(), "does not decrypt") || !strings.Contains(h.stderr, "refused (400): wrong hub key") {
		t.Fatalf("collector:\n%s\nhost:\n%s", col.log, h.stderr)
	}
}

// What a host sent once cannot be sent again by someone who recorded it.
func TestAReplayedRequestIsRefused(t *testing.T) {
	var captured []byte
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer recorder.Close()

	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	hubHost(t, "a.example.com", key, recorder.URL).run(0, "run")
	if len(captured) == 0 {
		t.Fatal("nothing was sent")
	}
	for i, want := range []int{http.StatusNoContent, http.StatusBadRequest} {
		resp, err := http.Post(col.url+"/v1/report", "application/json", bytes.NewReader(captured))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want || len(body) != 0 {
			t.Fatalf("send %d: %d %q\n%s", i+1, resp.StatusCode, body, col.log)
		}
	}
	if !strings.Contains(col.log.String(), "replayed request") {
		t.Fatalf("log:\n%s", col.log)
	}
}

func TestOneCollectorDownStillDeliversToTheOther(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	down := "http://" + freeAddr(t)
	h := hubHost(t, "a.example.com", key, down, col.url)
	h.run(0, "run")
	if col.stored(t, "a.example.com") == nil {
		t.Fatalf("the live collector got nothing:\n%s", h.stderr)
	}
	if !strings.Contains(h.stderr, "! Collector "+down+" not reached") || !strings.Contains(h.stderr, "Hub: 1 of 2") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
}

// The first run is a new UNHEALTHY finding (an alert), the second the same
// condition again (a heartbeat).
func TestAHeartbeatTheHubHoldsIsNotMailed(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	h := hubHost(t, "a.example.com", key, "http://"+freeAddr(t), col.url)

	h.run(0, "run")
	if msgs := h.relay.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Data, "[ACTION]") {
		t.Fatalf("the alert must be mailed whatever the hub did: %d message(s)\n%s", len(msgs), h.stderr)
	}
	h.run(0, "run")
	if n := len(h.relay.Messages()); n != 1 {
		t.Fatalf("heartbeat mailed although a collector holds the report: %d message(s)\n%s", n, h.stderr)
	}
	if !strings.Contains(h.stderr, "Heartbeat skipped — delivered to collector "+col.url) {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
	if rep := col.stored(t, "a.example.com"); rep == nil {
		t.Fatal("the collector has no report")
	}
}

func TestWithEveryCollectorDownTheHeartbeatIsMailed(t *testing.T) {
	c := newHost(t, relay(t))
	h := hubHost(t, "a.example.com", keygen(t, c), "http://"+freeAddr(t))
	h.run(0, "run")
	h.run(0, "run")
	msgs := h.relay.Messages()
	if len(msgs) != 2 || len(msgs[1].To) != 1 || msgs[1].To[0] != "ops@example.com" {
		t.Fatalf("want the heartbeat to the daily list, got %d message(s)\n%s", len(msgs), h.stderr)
	}
	if strings.Contains(h.stderr, "Heartbeat skipped") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
}

// With no alert list the alert goes to the daily list, so it looks like a
// heartbeat by its recipients. It is still an alert, and still mailed.
func TestAnAlertWithoutAnAlertListIsStillMailed(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	h := hubHost(t, "a.example.com", key, col.url)
	h.run(0, "config", "set", "email.alert_recipients=")
	h.run(0, "run")
	if msgs := h.relay.Messages(); len(msgs) != 1 || msgs[0].To[0] != "ops@example.com" {
		t.Fatalf("want the alert to the daily list, got %d message(s)\n%s", len(msgs), h.stderr)
	}
}

func TestHeartbeatNeverMailsOnlyAlerts(t *testing.T) {
	h := newHost(t, relay(t))
	h.run(0, "config", "set", "email.heartbeat=never")
	h.run(0, "run")
	h.run(0, "run")
	if n := len(h.relay.Messages()); n != 1 {
		t.Fatalf("want only the alert, got %d message(s)\n%s", n, h.stderr)
	}
	if !strings.Contains(h.stderr, "Heartbeat skipped — email.heartbeat is never") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
	if h.alertsState() == nil {
		t.Fatal("the alert history was not committed")
	}
}

func TestOnlyTheReportRouteAnswers(t *testing.T) {
	c := newHost(t, relay(t))
	col := startCollector(t, c, keygen(t, c))
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/"}, {http.MethodGet, "/v1/report"}, {http.MethodPost, "/"}, {http.MethodGet, "/debug/pprof/"},
	} {
		req, _ := http.NewRequest(r.method, col.url+r.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || len(body) != 0 {
			t.Errorf("%s %s: %d %q", r.method, r.path, resp.StatusCode, body)
		}
	}
}

func TestADryRunAndAReportSendNothing(t *testing.T) {
	var hits int
	var mu sync.Mutex
	recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer recorder.Close()
	c := newHost(t, relay(t))
	h := hubHost(t, "a.example.com", keygen(t, c), recorder.URL)
	h.run(0, "run", "--dry-run")
	h.run(0, "report")
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("%d reports sent by a preview", hits)
	}
}

func TestKeygenNeverOverwrites(t *testing.T) {
	h := newHost(t, relay(t))
	p := keygen(t, h)
	before, _ := os.ReadFile(p)
	h.run(1, "hub", "keygen", "--out", p)
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) || !strings.Contains(h.stderr, "already exists") {
		t.Fatalf("stderr: %s", h.stderr)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestACollectorMustBeEnabled(t *testing.T) {
	h := newHost(t, relay(t))
	h.run(0, "config", "set", "hub.key_file="+keygen(t, h))
	h.run(1, "collect")
	if !strings.Contains(h.stderr, "collector.enabled is false") {
		t.Fatalf("stderr: %s", h.stderr)
	}
}
