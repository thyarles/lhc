//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
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
	for _, want := range []string{"HOST", "VERSION", "a.example.com", "b.example.com", "unhealthy", "just now"} {
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
	// Nothing was mailed: no "daily list only", no subject of a mail that
	// does not exist.
	if !strings.Contains(h.stderr, ", left to the hub digest") || strings.Contains(h.stderr, "daily list only") || strings.Contains(h.stderr, "Subject:") {
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
	if strings.Contains(h.stderr, "Heartbeat skipped") || !strings.Contains(h.stderr, ", daily list only") || !strings.Contains(h.stderr, "Subject:") {
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
	if !strings.Contains(h.stderr, "Heartbeat skipped — email.heartbeat is never") ||
		!strings.Contains(h.stderr, ", not mailed (email.heartbeat is never)") || strings.Contains(h.stderr, "Subject:") {
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

// subject is a relayed message's decoded Subject line.
func subject(t *testing.T, data string) string {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	s, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTheDigestListsEveryHostInOneMail(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	c.run(0, "config", "set", "collector.peers=c2.example.com")
	for _, name := range []string{"a.example.com", "b.example.com"} {
		hubHost(t, name, key, col.url).run(0, "run")
	}

	c.run(0, "hub", "digest", "--dry-run")
	for _, want := range []string{"a.example.com", "b.example.com", "2 hosts · 0 OK · 2 UNHEALTHY", "c2.example.com", "SILENT, never reported here"} {
		if !strings.Contains(c.stdout, want) {
			t.Fatalf("dry run lacks %q:\n%s", want, c.stdout)
		}
	}
	if len(c.relay.Messages()) != 0 {
		t.Fatal("a dry run mailed the digest")
	}

	c.run(0, "hub", "digest")
	msgs := c.relay.Messages()
	if len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "ops@example.com" {
		t.Fatalf("want one digest to the daily list, got %+v", msgs)
	}
	want := "[hub] 2 hosts · 0 OK · 2 UNHEALTHY · via e2e.example.com · peer c2.example.com SILENT"
	if got := subject(t, msgs[0].Data); got != want {
		t.Fatalf("subject %q", got)
	}
	for _, host := range []string{"a.example.com", "b.example.com"} {
		if !strings.Contains(msgs[0].Data, host) {
			t.Fatalf("the digest does not name %s", host)
		}
	}
	if !strings.Contains(c.stdout, "Digest sent → ops@example.com") {
		t.Fatalf("stdout:\n%s", c.stdout)
	}
}

func TestADigestRecipientListReplacesTheDailyList(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	c.run(0, "config", "set", "collector.digest_recipients=hub@example.com")
	hubHost(t, "a.example.com", key, col.url).run(0, "run")
	c.run(0, "hub", "digest")
	if msgs := c.relay.Messages(); len(msgs) != 1 || len(msgs[0].To) != 1 || msgs[0].To[0] != "hub@example.com" {
		t.Fatalf("%+v", msgs)
	}
}

func TestADigestWithNoReportsSaysSo(t *testing.T) {
	h := newHost(t, relay(t))
	h.run(1, "hub", "digest", "--dry-run")
	if !strings.Contains(h.stderr, "no reports yet") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
}

// With one digest per hub, a collector's nightly run checks it could send
// the digest if its turn came: connect, EHLO, QUIT, never a message.
func TestACollectorChecksItsRelayEveryRun(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	c.run(0, "config", "set", "hub.collectors="+col.url, "hostname=c1.example.com")

	c.run(0, "report")
	if v := c.relay.Verbs(); len(v) != 0 {
		t.Fatalf("lhc report talked to the relay: %v", v)
	}
	c.run(0, "run", "--dry-run")
	if v := c.relay.Verbs(); len(v) != 0 {
		t.Fatalf("lhc run --dry-run talked to the relay: %v", v)
	}

	c.run(0, "run")
	if !strings.Contains(c.stderr, "Digest relay: reachable (127.0.0.1:") {
		t.Fatalf("stderr:\n%s", c.stderr)
	}
	// The probe sent no message: the only one is the run's own mail.
	if n := len(c.relay.Messages()); n != 1 {
		t.Fatalf("%d messages", n)
	}
	rep := col.stored(t, "c1.example.com")
	if !strings.Contains(fmt.Sprint(rep["sections"]), "Digest Relay") {
		t.Fatalf("no relay row in the stored report: %v", rep["sections"])
	}
	c.run(0, "hub", "status")
	for _, want := range []string{"Digest: one per hub · this collector is #1 of 1", "Relay: reachable"} {
		if !strings.Contains(c.stdout, want) {
			t.Fatalf("hub status lacks %q:\n%s", want, c.stdout)
		}
	}

	// The relay goes away: the run says so at CAUTION, with a stable alert.
	// The collector holds this run's report, so no mail is due: exit 0.
	c.run(0, "config", "set", "smtp.port=1")
	c.run(0, "run")
	if !strings.Contains(c.stderr, "Digest relay: connect 127.0.0.1:1") {
		t.Fatalf("stderr:\n%s", c.stderr)
	}
	rep = col.stored(t, "c1.example.com")
	if !strings.Contains(fmt.Sprint(rep["sections"]), "Digest relay unreachable from this collector") {
		t.Fatalf("no relay alert in the stored report: %v", rep["sections"])
	}
	c.run(0, "hub", "status")
	if !strings.Contains(c.stdout, "Relay: unreachable") {
		t.Fatalf("hub status:\n%s", c.stdout)
	}
}

func TestAHostThatIsNotACollectorNeverChecksTheRelay(t *testing.T) {
	h := newHost(t, relay(t))
	h.run(0, "run")
	if strings.Contains(h.stderr, "Digest relay") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
	if v := strings.Join(h.relay.Verbs(), " "); strings.Count(v, "EHLO") != 1 {
		t.Fatalf("more than the run's own mail: %s", v)
	}
}

// oomWorker is a host whose kernel log holds one container-limit OOM kill
// today; it has no crictl, so it can only send the pod's UID.
func oomWorker(t *testing.T, key, collector string) *host {
	t.Helper()
	h := hubHost(t, "w1.example.com", key, collector)
	day := time.Now().Format("Jan 02")
	slice := "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1a2b3c4d_5e6f_7a8b_9c0d_1e2f3a4b5c6d.slice/cri-containerd-4f1d0c3b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a392817060000.scope"
	journal := strings.Join([]string{
		day + " 00:01:40 w1 kernel: java invoked oom-killer: gfp_mask=0x6000c0(GFP_KERNEL), nodemask=(null), order=0, oom_score_adj=985",
		day + " 00:01:40 w1 kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=x,mems_allowed=0,oom_memcg=" + slice + ",task_memcg=" + slice + ",task=java,pid=12345,uid=1000",
		day + " 00:01:40 w1 kernel: Memory cgroup out of memory: Killed process 12345 (java) total-vm:8123456kB, anon-rss:524000kB",
	}, "\n")
	if err := os.WriteFile(filepath.Join(h.dir, "bin", "journalctl"), []byte("#!/bin/sh\ncat <<'EOF'\n"+journal+"\nEOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.run(0, "config", "set", "checks.logs.enabled=true")
	return h
}

// kubectlOn gives a collector a fake kubectl that logs its arguments and
// a kubeconfig named by checks.kubernetes.kubeconfig.
func kubectlOn(t *testing.T, c *host) (calls string) {
	t.Helper()
	calls = filepath.Join(c.dir, "kubectl.calls")
	pods := `{"items":[{"metadata":{"namespace":"team-a","name":"api-6b7c9d-x2k4p","uid":"1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d","ownerReferences":[{"kind":"ReplicaSet","name":"api-6b7c9d"}]},"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"512Mi"}}}]},"status":{}}]}`
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\ncat <<'EOF'\n" + pods + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(c.dir, "bin", "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(c.dir, "kubeconfig.yaml")
	if err := os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.run(0, "config", "set", "checks.kubernetes.kubeconfig="+kc)
	return calls
}

func TestTheDigestNamesTheOOMKilledPodWithItsLimit(t *testing.T) {
	c := newHost(t, relay(t))
	key := keygen(t, c)
	col := startCollector(t, c, key)
	calls := kubectlOn(t, c)
	w := oomWorker(t, key, col.url)
	w.run(0, "run")
	b, _ := json.Marshal(col.stored(t, "w1.example.com"))
	if r := string(b); !strings.Contains(r, "OOM Killer: 1 kill(s) today") || !strings.Contains(r, `"reason":"oom_killed","uid":"1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"`) {
		t.Fatalf("stored report:\n%s", r)
	}

	c.run(0, "hub", "status")
	if !strings.Contains(c.stdout, "Kubernetes lookup: on (kubeconfig "+filepath.Join(c.dir, "kubeconfig.yaml")+")") {
		t.Fatalf("hub status:\n%s", c.stdout)
	}
	c.run(0, "hub", "digest", "--dry-run")
	flat := strings.Join(strings.Fields(c.stdout), " ")
	if !strings.Contains(flat, "OOM team-a/api-6b7c9d-x2k4p · container app · 1 kill · limit 512Mi · Deployment team-a/api") {
		t.Fatalf("digest:\n%s", c.stdout)
	}
	// One call, and only ever a get.
	got := strings.TrimSpace(readFile(t, calls))
	if want := "--kubeconfig " + filepath.Join(c.dir, "kubeconfig.yaml") + " --request-timeout=15s get pods --all-namespaces -o json"; got != want {
		t.Fatalf("kubectl ran %q, want %q", got, want)
	}

	// Turned off: no kubectl at all, the lines say what the host sent.
	c.run(0, "config", "set", "collector.kubernetes_lookup=false")
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	c.run(0, "hub", "digest", "--dry-run")
	flat = strings.Join(strings.Fields(c.stdout), " ")
	if !strings.Contains(flat, "OOM pod UID 1a2b3c4d-… · java · 1 kill · container limit") {
		t.Fatalf("digest without the lookup:\n%s", c.stdout)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("kubectl ran with the lookup off: %s", readFile(t, calls))
	}
	c.run(0, "hub", "status")
	if !strings.Contains(c.stdout, "Kubernetes lookup: off (collector.kubernetes_lookup: false)") {
		t.Fatalf("hub status:\n%s", c.stdout)
	}
}
