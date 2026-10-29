package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/notify/smtp/smtptest"
)

// member is one collector of a test hub: its app, store, log and receiver.
type member struct {
	a     *app
	store hub.Store
	log   *bytes.Buffer
	srv   *httptest.Server
	g     *digester
}

// rota builds a hub of collectors c1..cN with digest_mode one. Each mails
// through relays[i] and receives notices on a real HTTP receiver. Every
// collector holds the same report from a.example.com.
func rota(t *testing.T, relays []*smtptest.Server, clock *time.Time) []*member {
	t.Helper()
	dir := t.TempDir()
	file, err := hub.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "hub.key")
	if err := os.WriteFile(keyFile, file, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := hub.ParseKey(file)
	if err != nil {
		t.Fatal(err)
	}
	n := len(relays)
	ms := make([]*member, n)
	var names, urls []string
	for i := range n {
		name := fmt.Sprintf("c%d.example.com", i+1)
		store := hub.Store{Dir: filepath.Join(dir, name, "state", "hub")}
		recv := hub.NewReceiver(key, 1<<20, store, func() time.Time { return *clock }, func(string, ...any) {})
		srv := httptest.NewServer(recv)
		t.Cleanup(srv.Close)
		ms[i] = &member{store: store, srv: srv, log: &bytes.Buffer{}}
		names, urls = append(names, name), append(urls, srv.URL)
	}
	for i, m := range ms {
		var peers []string
		for j, p := range names {
			if j != i {
				peers = append(peers, `"`+p+`"`)
			}
		}
		cfg := filepath.Join(dir, names[i], "config.yaml")
		if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`hostname: %s
smtp:
  host: 127.0.0.1
  port: %d
email:
  daily_recipients: [ops@example.com]
paths:
  state_dir: %s
hub:
  collectors: [%s]
  key_file: %s
collector:
  enabled: true
  digest_time: "09:00"
  peers: [%s]
`, names[i], relays[i].Port(), filepath.Join(dir, names[i], "state"), strings.Join(urls, ", "), keyFile, strings.Join(peers, ", "))
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		m.a = newApp(m.log, m.log)
		m.a.configFlag = cfg
		m.a.now = func() time.Time { return *clock }
		if err := m.a.load(); err != nil {
			t.Fatal(err)
		}
		rep, _ := json.Marshal(map[string]any{"schema": 1, "host": "a.example.com", "overall": "ok"})
		if err := m.store.Put("a.example.com", hub.Stored{ReceivedAt: *clock, Report: rep}); err != nil {
			t.Fatal(err)
		}
		logf := func(format string, args ...any) { _, _ = fmt.Fprintf(m.log, format+"\n", args...) }
		m.g = newDigester(m.a, m.store, logf)
	}
	return ms
}

// minutes ticks every collector once a minute.
func minutes(ms []*member, clock *time.Time, n int) {
	for range n {
		*clock = clock.Add(time.Minute)
		for _, m := range ms {
			m.g.tick(context.Background())
		}
	}
}

func logs(ms []*member) string {
	var b strings.Builder
	for i, m := range ms {
		fmt.Fprintf(&b, "--- c%d\n%s", i+1, m.log)
	}
	return b.String()
}

func TestOneDigestPerHub(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay, relay}, &clock)

	minutes(ms, &clock, 120) // to 10:00
	if n := len(relay.Messages()); n != 1 {
		t.Fatalf("%d digests from one hub:\n%s", n, logs(ms))
	}
	if !strings.Contains(relay.Messages()[0].Data, "Sent by c1.example.com =C2=B7 standby c2.example.com, c3.example.com") &&
		!strings.Contains(relay.Messages()[0].Data, "Sent by c1.example.com · standby c2.example.com, c3.example.com") {
		t.Fatalf("no sender line:\n%s", relay.Messages()[0].Data)
	}
	if !strings.Contains(ms[0].log.String(), "digest notice → 3 of 3 collector(s)") {
		t.Fatalf("c1 told nobody:\n%s", logs(ms))
	}
	for _, m := range ms[1:] {
		if !strings.Contains(m.log.String(), "digest skipped: sent by c1.example.com at 09:00") {
			t.Fatalf("a standby did not stand by:\n%s", logs(ms))
		}
		// Saved anyway, so its own next digest compares with today's.
		if st := m.store.LoadDigest(); st.Hosts["a.example.com"] != "ok" {
			t.Fatalf("standby state %+v", st)
		}
	}

	minutes(ms, &clock, 24*60) // the next day: the same again
	if n := len(relay.Messages()); n != 2 {
		t.Fatalf("%d digests after two days:\n%s", n, logs(ms))
	}
}

// The first collector's relay refuses: the second takes over at 09:15, and
// neither the first one's 09:30 retry nor the third sends a second copy.
func TestTheNextCollectorTakesOver(t *testing.T) {
	broken := newRelay(t, func(s *smtptest.Server) { s.TempFailData = 100 })
	good := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{broken, good, good}, &clock)

	minutes(ms, &clock, 120)
	if n := len(good.Messages()); n != 1 {
		t.Fatalf("%d digests:\n%s", n, logs(ms))
	}
	data := good.Messages()[0].Data
	if !strings.Contains(data, "no digest from c1.example.com by 09:15") {
		t.Fatalf("no takeover line:\n%s", data)
	}
	if !strings.Contains(ms[1].log.String(), "digest sent →") || !strings.Contains(ms[0].log.String(), "digest skipped: sent by c2.example.com at 09:15") {
		t.Fatalf("logs:\n%s", logs(ms))
	}
	if !strings.Contains(ms[2].log.String(), "digest skipped: sent by c2.example.com") {
		t.Fatalf("logs:\n%s", logs(ms))
	}
}

// A notice that does not arrive costs one mail too many, never none.
func TestALostNoticeMeansTwoMailsNotNone(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay}, &clock)
	ms[1].srv.Close() // c2's receiver is down at 09:00

	minutes(ms, &clock, 120)
	if n := len(relay.Messages()); n != 2 {
		t.Fatalf("%d digests:\n%s", n, logs(ms))
	}
	if !strings.Contains(ms[0].log.String(), "digest notice → 1 of 2 collector(s)") {
		t.Fatalf("logs:\n%s", logs(ms))
	}
}

// A collector on an older lhc answers 404 on the notice path: it will send
// its own digest, as before, and the log says why.
func TestAnOlderPeerIsNamedInTheLog(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay}, &clock)
	ms[1].srv.Config.Handler = http.NotFoundHandler()

	minutes(ms, &clock, 60)
	if !strings.Contains(ms[0].log.String(), "does not take digest notices (older lhc)") {
		t.Fatalf("logs:\n%s", logs(ms))
	}
}

func TestEachCollectorSendsItsOwnInModeEach(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay, relay}, &clock)
	for _, m := range ms {
		m.a.cfg.Collector.DigestMode = "each"
	}
	minutes(ms, &clock, 120)
	if n := len(relay.Messages()); n != 3 {
		t.Fatalf("%d digests:\n%s", n, logs(ms))
	}
	for _, msg := range relay.Messages() {
		if strings.Contains(msg.Data, "Sent by") || strings.Contains(msg.Data, "standby") {
			t.Fatalf("a sender line in mode each:\n%s", msg.Data)
		}
	}
	for _, m := range ms {
		if strings.Contains(m.log.String(), "notice") {
			t.Fatalf("a notice in mode each:\n%s", logs(ms))
		}
	}
}

// The upgrade from v0.11 happens after that morning's digests went out, one
// per collector, at 09:00. The second collector's turn is now 09:15: it
// must not send the day's digest again.
func TestTheUpgradeDayGetsNoSecondDigest(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 10, 0, 0, 0, time.Local)
	sent := time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay, relay}, &clock)
	for _, m := range ms {
		if err := m.store.SaveDigest(hub.DigestState{Slot: sent, Hosts: map[string]string{"a.example.com": "ok"}}); err != nil {
			t.Fatal(err)
		}
		logf := func(format string, args ...any) { _, _ = fmt.Fprintf(m.log, format+"\n", args...) }
		m.g = newDigester(m.a, m.store, logf)
	}
	minutes(ms, &clock, 120)
	if n := len(relay.Messages()); n != 0 {
		t.Fatalf("%d digest(s) on the upgrade day:\n%s", n, logs(ms))
	}
	minutes(ms, &clock, 24*60)
	if n := len(relay.Messages()); n != 1 {
		t.Fatalf("%d digest(s) the next day:\n%s", n, logs(ms))
	}
}
