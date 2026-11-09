package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/notify/smtp/smtptest"
)

// The cluster a fake kubectl answers for: team-a's api, annotated, with
// two pods on two workers.
const (
	ownerPodsJSON = `{"items":[
{"metadata":{"namespace":"team-a","name":"api-6b7c9d8f5-x2k4p","uid":"11111111-0000-0000-0000-000000000001","ownerReferences":[{"kind":"ReplicaSet","name":"api-6b7c9d8f5"}]},"spec":{"nodeName":"w1","containers":[{"name":"app","resources":{"limits":{"memory":"512Mi"}}}]},"status":{}},
{"metadata":{"namespace":"team-a","name":"api-6b7c9d8f5-q8w2e","uid":"11111111-0000-0000-0000-000000000002","ownerReferences":[{"kind":"ReplicaSet","name":"api-6b7c9d8f5"}]},"spec":{"nodeName":"w2","containers":[{"name":"app","resources":{"limits":{"memory":"512Mi"}}}]},"status":{}}]}`
	ownerWorkloadsJSON  = `{"items":[{"kind":"Deployment","metadata":{"namespace":"team-a","name":"api","annotations":{"lhc/notify":"dev.one, dev@example.org"}}}]}`
	ownerNamespacesJSON = `{"items":[{"kind":"Namespace","metadata":{"name":"team-a"}}]}`
)

// fakeKubectl puts a kubectl first on the runner's PATH that logs its
// arguments to calls and answers get pods, workloads and namespaces.
func fakeKubectl(t *testing.T, workloads string) (dir, calls string) {
	t.Helper()
	dir = t.TempDir()
	calls = filepath.Join(dir, "kubectl.calls")
	script := `#!/bin/sh
echo "$@" >> ` + calls + `
case "$*" in
*" get pods "*) cat <<'EOF'
` + ownerPodsJSON + `
EOF
;;
*" get deployments,statefulsets,daemonsets "*) cat <<'EOF'
` + workloads + `
EOF
;;
*" get namespaces "*) cat <<'EOF'
` + ownerNamespacesJSON + `
EOF
;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig.yaml"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LHC_PATH_OVERRIDE", dir)
	return dir, calls
}

// withOwnerMail gives a collector smtp.from, the fake cluster's
// kubeconfig, and two workers' reports of api's kills: 6 and 5.
func withOwnerMail(t *testing.T, a *app, store hub.Store, kubeDir string, clock time.Time, extra string) {
	t.Helper()
	b, err := os.ReadFile(a.configFlag)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(b), "smtp:\n", "smtp:\n  from: lhc@example.com\n", 1)
	body = strings.Replace(body, "collector:\n", "collector:\n"+extra, 1)
	body += "checks:\n  kubernetes:\n    kubeconfig: " + filepath.Join(kubeDir, "kubeconfig.yaml") + "\n"
	if err := os.WriteFile(a.configFlag, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfgPath = ""
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	for host, pod := range map[string][2]any{
		"w1.example.com": {"11111111-0000-0000-0000-000000000001", 6},
		"w2.example.com": {"11111111-0000-0000-0000-000000000002", 5},
	} {
		rep, _ := json.Marshal(map[string]any{
			"schema": 1, "host": host, "overall": "caution",
			"sections": []map[string]any{{
				"name":   "logs",
				"alerts": []map[string]any{{"status": "caution", "msg": "OOM Killer: 6 kill(s) today"}},
				"pods":   []map[string]any{{"uid": pod[0], "reason": "oom_killed", "count": pod[1]}},
			}},
		})
		if err := store.Put(host, hub.Stored{ReceivedAt: clock, Report: rep}); err != nil {
			t.Fatal(err)
		}
	}
}

// ownerMails are the messages the relay took for dev.one.
func ownerMails(relay *smtptest.Server) []smtptest.Message {
	var out []smtptest.Message
	for _, m := range relay.Messages() {
		if strings.Join(m.To, ",") == "dev.one@example.com" {
			out = append(out, m)
		}
	}
	return out
}

func TestTheOwnersAreMailedAfterTheDigestThenReminded(t *testing.T) {
	kube, calls := fakeKubectl(t, ownerWorkloadsJSON)
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay}, &clock)
	for _, m := range ms {
		withOwnerMail(t, m.a, m.store, kube, clock, "")
	}

	minutes(ms, &clock, 120) // to 10:00
	if n := len(relay.Messages()); n != 2 {
		t.Fatalf("%d messages, want the digest and one owner mail:\n%s", n, logs(ms))
	}
	digest, owner := relay.Messages()[0], relay.Messages()[1]
	if strings.Join(digest.To, ",") != "ops@example.com" || strings.Join(owner.To, ",") != "dev.one@example.com" {
		t.Fatalf("order or recipients: %q then %q", digest.To, owner.To)
	}
	flat := strings.Join(strings.Fields(qpDecode(digest.Data)), " ")
	if !strings.Contains(flat, "Deployment team-a/api · told dev.one@example.com") {
		t.Fatalf("digest:\n%s", flat)
	}
	text := qpDecode(owner.Data)
	for _, want := range []string{"Subject: =?utf-8?q?[lhc]_OOM_kills:_team-a/api", "team-a/api-6b7c9d8f5-x2k4p · container app · 6 kills · limit 512Mi", "node w2", "Content-Type: text/plain"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in the owner mail:\n%s", want, text)
		}
	}
	if strings.Contains(text, "example.org") || strings.Contains(text, "ops@") {
		t.Fatalf("the owner mail names someone else:\n%s", text)
	}
	if owner.From != "lhc@example.com" {
		t.Fatalf("from %q", owner.From)
	}
	l := logs(ms)
	if !strings.Contains(ms[0].log.String(), `owner mail: Deployment team-a/api: "dev@example.org" skipped (not @example.com) on Deployment team-a/api`) ||
		!strings.Contains(ms[0].log.String(), "owner mail → dev.one@example.com (Deployment team-a/api, 11 kills)") ||
		strings.Contains(ms[1].log.String(), "owner mail") {
		t.Fatalf("logs:\n%s", l)
	}
	for c := range strings.SplitSeq(strings.TrimSpace(readCalls(t, calls)), "\n") {
		if !strings.Contains(c, " get ") || strings.Contains(c, " create ") {
			t.Fatalf("kubectl ran %q", c)
		}
	}

	// The next day: the digest, and no owner mail.
	refresh(t, ms, clock.Add(23*time.Hour))
	minutes(ms, &clock, 24*60)
	if n := len(ownerMails(relay)); n != 1 {
		t.Fatalf("%d owner mails the next day:\n%s", n, logs(ms))
	}
	// A week after the first: a reminder.
	for range 6 {
		refresh(t, ms, clock.Add(23*time.Hour))
		minutes(ms, &clock, 24*60)
	}
	if n := len(ownerMails(relay)); n != 2 {
		t.Fatalf("%d owner mails after a week:\n%s", n, logs(ms))
	}
}

// refresh has both workers report again at, so their kills count today.
func refresh(t *testing.T, ms []*member, at time.Time) {
	t.Helper()
	for _, m := range ms {
		for _, h := range []string{"w1.example.com", "w2.example.com"} {
			p := filepath.Join(m.store.Dir, h+".json")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var st hub.Stored
			if err := json.Unmarshal(b, &st); err != nil {
				t.Fatal(err)
			}
			st.ReceivedAt = at
			if err := m.store.Put(h, st); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestARefusedOwnerMailIsTriedWithTheNextDigest(t *testing.T) {
	kube, _ := fakeKubectl(t, ownerWorkloadsJSON)
	relay := newRelay(t, func(s *smtptest.Server) { s.RejectRcptTo = "dev.one@" })
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay}, &clock)
	withOwnerMail(t, ms[0].a, ms[0].store, kube, clock, "")

	minutes(ms, &clock, 120)
	if n := len(relay.Messages()); n != 1 {
		t.Fatalf("%d messages, want the digest only, once:\n%s", n, logs(ms))
	}
	if !strings.Contains(ms[0].log.String(), "owner mail: Deployment team-a/api not sent, trying again with the next digest") {
		t.Fatalf("logs:\n%s", logs(ms))
	}
	if st := ms[0].store.LoadOwners(); len(st.Mailed) != 0 {
		t.Fatalf("a refused mail was recorded: %v", st.Mailed)
	}
	// The next day, through a relay that takes it.
	good := newRelay(t)
	b, _ := os.ReadFile(ms[0].a.configFlag)
	body := strings.Replace(string(b), fmt.Sprintf("port: %d\n", relay.Port()), fmt.Sprintf("port: %d\n", good.Port()), 1)
	if err := os.WriteFile(ms[0].a.configFlag, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ms[0].a.load(); err != nil {
		t.Fatal(err)
	}
	refresh(t, ms, clock.Add(23*time.Hour))
	minutes(ms, &clock, 24*60)
	if n := len(ownerMails(good)); n != 1 {
		t.Fatalf("%d owner mails the next day:\n%s", n, logs(ms))
	}
}

func TestOnlyTheCollectorThatSendsTheDigestMailsOwners(t *testing.T) {
	kube, _ := fakeKubectl(t, ownerWorkloadsJSON)
	broken := newRelay(t, func(s *smtptest.Server) { s.TempFailData = 100 })
	good := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{broken, good, good}, &clock)
	for _, m := range ms {
		withOwnerMail(t, m.a, m.store, kube, clock, "")
	}
	minutes(ms, &clock, 120)
	// c1's relay refused; c2 took over, sent the digest and the owner mail;
	// c1 and c3 hold c2's notice and send neither.
	if n := len(ownerMails(good)); n != 1 || len(good.Messages()) != 2 {
		t.Fatalf("%d owner mails, %d messages:\n%s", n, len(good.Messages()), logs(ms))
	}
	for _, i := range []int{0, 2} {
		if strings.Contains(ms[i].log.String(), "owner mail →") {
			t.Fatalf("c%d mailed owners:\n%s", i+1, logs(ms))
		}
	}
}

func TestWithDigestModeEachOnlyTheFirstCollectorMailsOwners(t *testing.T) {
	kube, _ := fakeKubectl(t, ownerWorkloadsJSON)
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay, relay}, &clock)
	for _, m := range ms {
		withOwnerMail(t, m.a, m.store, kube, clock, "  digest_mode: each\n")
	}
	minutes(ms, &clock, 120)
	if n := len(ownerMails(relay)); n != 1 || len(relay.Messages()) != 3 {
		t.Fatalf("%d owner mails, %d messages:\n%s", n, len(relay.Messages()), logs(ms))
	}
	if !strings.Contains(ms[0].log.String(), "owner mail →") {
		t.Fatalf("c1 did not mail owners:\n%s", logs(ms))
	}
}

func TestOwnerMailOffOrWithoutADomainRunsNoExtraKubectl(t *testing.T) {
	for name, extra := range map[string]string{
		"off":       "  owner_notify_min_kills: 0\n",
		"no domain": "",
		"too few":   "  owner_notify_min_kills: 12\n",
	} {
		t.Run(name, func(t *testing.T) {
			kube, calls := fakeKubectl(t, ownerWorkloadsJSON)
			relay := newRelay(t)
			clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
			ms := rota(t, []*smtptest.Server{relay}, &clock)
			withOwnerMail(t, ms[0].a, ms[0].store, kube, clock, extra)
			if name == "no domain" {
				b, _ := os.ReadFile(ms[0].a.configFlag)
				_ = os.WriteFile(ms[0].a.configFlag, []byte(strings.Replace(string(b), "  from: lhc@example.com\n", "", 1)), 0o600)
				ms[0].a.cfgPath = ""
				if err := ms[0].a.load(); err != nil {
					t.Fatal(err)
				}
			}
			minutes(ms, &clock, 120)
			if n := len(relay.Messages()); n != 1 {
				t.Fatalf("%d messages:\n%s", n, logs(ms))
			}
			if got := strings.TrimSpace(readCalls(t, calls)); strings.Contains(got, "get deployments") || strings.Contains(got, "get namespaces") || !strings.Contains(got, "get pods") {
				t.Fatalf("kubectl calls:\n%s", got)
			}
			if strings.Contains(qpDecode(relay.Messages()[0].Data), "told") || strings.Contains(qpDecode(relay.Messages()[0].Data), "no owner named") {
				t.Fatalf("digest:\n%s", qpDecode(relay.Messages()[0].Data))
			}
		})
	}
}

func TestNoAnnotationMeansNoMailAndTheDigestSaysSo(t *testing.T) {
	kube, _ := fakeKubectl(t, `{"items":[{"kind":"Deployment","metadata":{"namespace":"team-a","name":"api"}}]}`)
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	ms := rota(t, []*smtptest.Server{relay}, &clock)
	withOwnerMail(t, ms[0].a, ms[0].store, kube, clock, "")
	minutes(ms, &clock, 120)
	if n := len(relay.Messages()); n != 1 {
		t.Fatalf("%d messages:\n%s", n, logs(ms))
	}
	flat := strings.Join(strings.Fields(qpDecode(relay.Messages()[0].Data)), " ")
	if !strings.Contains(flat, "Deployment team-a/api · no owner named") {
		t.Fatalf("digest:\n%s", flat)
	}
	if !strings.Contains(ms[0].log.String(), "owner mail: Deployment team-a/api: 11 kills, no owner named") {
		t.Fatalf("logs:\n%s", logs(ms))
	}
}

func TestADigestByHandMailsNoOwnerAndDryRunSaysWhatWouldGo(t *testing.T) {
	kube, _ := fakeKubectl(t, ownerWorkloadsJSON)
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	a, store, out := collectorApp(t, relay, &clock)
	withOwnerMail(t, a, store, kube, clock, "")

	cmd := newHubDigestCmd(a)
	cmd.SetArgs([]string{"--dry-run"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Owner mail (none is sent by --dry-run or by hand):",
		"  → dev.one@example.com: [lhc] OOM kills: team-a/api · 11 today (annotation on Deployment team-a/api)",
		`    ! "dev@example.org" skipped (not @example.com) on Deployment team-a/api`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	if len(relay.Messages()) != 0 {
		t.Fatal("--dry-run sent mail")
	}

	cmd = newHubDigestCmd(a)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(relay.Messages()); n != 1 || len(ownerMails(relay)) != 0 {
		t.Fatalf("%d messages by hand, %d to owners", n, len(ownerMails(relay)))
	}
	if st := store.LoadOwners(); len(st.Mailed) != 0 {
		t.Fatalf("a digest by hand touched the state: %v", st.Mailed)
	}
}

func TestHubStatusSaysWhereOwnerMailGoes(t *testing.T) {
	relay := newRelay(t)
	clock := time.Date(2026, 10, 28, 8, 0, 0, 0, time.Local)
	a, _, _ := collectorApp(t, relay, &clock)
	if got := a.ownerLine(); got != "  Owner mail: unavailable (no mail domain in smtp.from or smtp.host)" {
		t.Fatalf("an IP host: %q", got)
	}
	a.cfg.SMTP.Host = "relay.example.com"
	if got := a.ownerLine(); got != "  Owner mail: to @example.com (from smtp.host)" {
		t.Fatalf("%q", got)
	}
	a.cfg.SMTP.From = "Health <lhc@example.net>"
	if got := a.ownerLine(); got != "  Owner mail: to @example.net (from smtp.from)" {
		t.Fatalf("%q", got)
	}
	a.cfg.Collector.OwnerNotifyMinKills = 0
	if got := a.ownerLine(); got != "  Owner mail: off (collector.owner_notify_min_kills: 0)" {
		t.Fatalf("%q", got)
	}
}

func readCalls(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// qpDecode is a message as its reader sees it: soft line breaks joined,
// =C2=B7 back to ·.
func qpDecode(data string) string {
	b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(data)))
	if err != nil {
		return data
	}
	return string(b)
}
