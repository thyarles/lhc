package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/notify/smtp/smtptest"
)

// collectorApp is a collector holding one report from a.example.com, with
// a clock the test moves.
func collectorApp(t *testing.T, relay *smtptest.Server, clock *time.Time) (*app, hub.Store, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`hostname: c1.example.com
smtp:
  host: 127.0.0.1
  port: %d
email:
  daily_recipients: [ops@example.com]
paths:
  state_dir: %s
collector:
  enabled: true
  digest_time: "09:00"
`, relay.Port(), filepath.Join(dir, "state"))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := newApp(&out, &out)
	a.configFlag = cfg
	a.now = func() time.Time { return *clock }
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	store := a.hubStore()
	rep, _ := json.Marshal(map[string]any{"schema": 1, "host": "a.example.com", "overall": "ok"})
	if err := store.Put("a.example.com", hub.Stored{ReceivedAt: *clock, Report: rep}); err != nil {
		t.Fatal(err)
	}
	return a, store, &out
}

func newRelay(t *testing.T, opts ...func(*smtptest.Server)) *smtptest.Server {
	t.Helper()
	s, err := smtptest.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestTheScheduledDigestGoesOutOncePerDay(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 21, 15, 0, 0, 0, time.Local)
	a, store, log := collectorApp(t, relay, &clock)
	g := newDigester(a, store, a.printf)
	ctx := context.Background()

	g.tick(ctx) // installed at 15:00: the first digest is tomorrow's
	clock = clock.Add(17*time.Hour + 59*time.Minute)
	g.tick(ctx) // 08:59
	if n := len(relay.Messages()); n != 0 {
		t.Fatalf("%d digest(s) before the first digest_time:\n%s", n, log)
	}
	clock = clock.Add(time.Minute)
	g.tick(ctx) // 09:00
	for range 60 {
		clock = clock.Add(time.Minute)
		g.tick(ctx)
	}
	if n := len(relay.Messages()); n != 1 {
		t.Fatalf("%d digests on one day:\n%s", n, log)
	}
	if st := store.LoadDigest(); st.Slot.Hour() != 9 || st.Hosts["a.example.com"] != "ok" {
		t.Fatalf("state %+v", st)
	}

	// A restart the same day reads the state and sends nothing more.
	g = newDigester(a, store, a.printf)
	g.tick(ctx)
	clock = clock.Add(23 * time.Hour)
	g.tick(ctx)
	if n := len(relay.Messages()); n != 2 {
		t.Fatalf("want the next day's digest, %d sent:\n%s", n, log)
	}
	if !strings.Contains(log.String(), "digest sent → ops@example.com (1 host · 1 OK)") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestARefusedDigestIsRetriedEvery30Minutes(t *testing.T) {
	relay := newRelay(t, func(s *smtptest.Server) { s.TempFailData = 1 })
	clock := time.Date(2026, 10, 21, 8, 0, 0, 0, time.Local)
	a, store, log := collectorApp(t, relay, &clock)
	g := newDigester(a, store, a.printf)
	ctx := context.Background()

	clock = clock.Add(time.Hour)
	g.tick(ctx) // 09:00, refused
	if !strings.Contains(log.String(), "digest not sent, trying again in 30m0s") {
		t.Fatalf("log:\n%s", log)
	}
	for range 29 {
		clock = clock.Add(time.Minute)
		g.tick(ctx)
	}
	if n := len(relay.Messages()); n != 0 {
		t.Fatalf("retried before 30 minutes: %d", n)
	}
	clock = clock.Add(time.Minute)
	g.tick(ctx) // 09:30
	g.tick(ctx)
	if n := len(relay.Messages()); n != 1 {
		t.Fatalf("%d messages after the retry:\n%s", n, log)
	}
}
