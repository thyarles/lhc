package kubernetes

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

// ─── Parsing: kubectl's output has moved twice ─────────────────────────────

func TestAgeParsing(t *testing.T) {
	cases := []struct {
		age  string
		secs float64
	}{
		{"45s", 45},
		{"5m", 300},
		{"5m30s", 330},
		{"3h20m", 3*3600 + 20*60},
		{"36h", 36 * 3600},
		{"5d3h", 5*86400 + 3*3600},
		{"2y40d", 2*31536000 + 40*86400},
	}
	for _, c := range cases {
		if got := ageSeconds(c.age); got != c.secs {
			t.Errorf("ageSeconds(%q) = %v, want %v", c.age, got, c.secs)
		}
	}
}

func TestUnparseableAgeIsNotUsedToEscalate(t *testing.T) {
	if got := ageSeconds("<unknown>"); got != -1 {
		t.Fatalf("got %v", got)
	}
}

func TestRestartsColumnBothShapes(t *testing.T) {
	cases := []struct {
		name, line string
		restarts   int
		since, age string
	}{
		{"kubectl < 1.23: bare int, six tokens", "default api-1 1/2 CrashLoopBackOff 12 3d4h", 12, "", "3d4h"},
		{"kubectl >= 1.23: '(5m ago)', eight tokens", "default api-1 1/2 CrashLoopBackOff 12 (5m ago) 3d4h", 12, "5m ago", "3d4h"},
		{"no restarts", "kube-system coredns-x 1/1 Running 0 5d", 0, "", "5d"},
	}
	for _, c := range cases {
		p, ok := parsePodLine(c.line)
		if !ok || p.restarts != c.restarts || p.since != c.since || p.age != c.age || p.status != strings.TrimSpace(p.status) {
			t.Errorf("%s: %+v", c.name, p)
		}
	}
}

func TestReadyFractionIsSplit(t *testing.T) {
	p, _ := parsePodLine("default api-1 1/2 Running 0 5d")
	if p.readyN != 1 || p.readyOf != 2 {
		t.Fatalf("%+v", p)
	}
}

// ─── Pods: this is where the noise lives ───────────────────────────────────

// pods runs the check on a single-node cluster with the given pod list.
func pods(t *testing.T, e *checktest.Env, lines ...string) *check.Section {
	t.Helper()
	cluster(e, oneNode)
	e.Fake.Expect(podsCmd, strings.Join(lines, "\n"))
	return e.Run(Check{})
}

func TestPodsThatMustNotAlert(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		// The single biggest Kubernetes noise source: install pods that
		// finished months ago and sit in the list forever.
		{"completed helm and cronjob pods", []string{
			"kube-system helm-install-traefik-2xk9p 0/1 Completed 0 13d",
			"kube-system helm-install-traefik-crd-p4b2z 0/1 Completed 0 13d",
			"default backup-cron-28912 0/1 Completed 0 6h",
			"kube-system coredns-6799fbcd5-abcde 1/1 Running 0 13d",
		}},
		// Evicted objects persist until GC; last month's eviction is not
		// today's news.
		{"month-old evicted pod", []string{"default legacy-abc123 0/1 Evicted 0 27d"}},
		{"young pending pod is the scheduler working", []string{"monitoring prom-0 0/1 Pending 0 2m30s"}},
		// A rolling deploy makes a new pod name. It has no history, so it
		// cannot have grown, otherwise every deploy would alert.
		{"pod seen for the first time has no growth", []string{"default worker-2 1/1 Running 9 5m"}},
		{"unknown status never invents an incident", []string{"default weird-1 0/1 SomeFuturePhase 0 5d"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := pods(t, newEnv(t), c.lines...)
			checktest.NoAlerts(t, s)
			if s.Status >= check.Caution {
				t.Fatalf("section status %v", s.Status)
			}
		})
	}
}

func TestARecentEvictionStillAlerts(t *testing.T) {
	alertCount(t, pods(t, newEnv(t), "default legacy-abc123 0/1 Evicted 0 12m"), 1)
}

func TestPendingPastTheThresholdAlerts(t *testing.T) {
	alertCount(t, pods(t, newEnv(t), "monitoring prom-0 0/1 Pending 0 3h"), 1)
}

func TestAPodRestartingWhenCronFiredIsNotAnIncident(t *testing.T) {
	// 0/1 Running for a few seconds at 07:00 is a rolling restart, not an
	// outage. Only a pod still degraded on the NEXT run is news.
	e := newEnv(t)
	s := pods(t, e, "default api-7d8f9c4b5-qz2lm 0/1 Running 0 30s")
	checktest.NoAlerts(t, s)
	var saved []string
	if !e.Loaded(t, "degraded_pods", &saved) || !slices.Equal(saved, []string{"default/api-7d8f9c4b5-qz2lm"}) {
		t.Fatalf("saved %q", saved)
	}
}

func TestAPodDegradedOnTwoRunsDoesAlert(t *testing.T) {
	e := newEnv(t)
	e.Seed(t, "degraded_pods", []string{"default/api-7d8f9c4b5-qz2lm"})
	alertCount(t, pods(t, e, "default api-7d8f9c4b5-qz2lm 0/1 Running 0 2d"), 1)
}

func TestCrashLoopAlerts(t *testing.T) {
	s := pods(t, newEnv(t), "default api-1 1/2 CrashLoopBackOff 12 (5m ago) 3d4h")
	if got := statusOf(t, s, "default/api-1"); got != check.Caution {
		t.Fatalf("status %v", got)
	}
	if !strings.Contains(s.AlertMsgs()[0], "default/api-1") {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
	if r := checktest.Rows(s)["default/api-1"]; r.Value != "1/2 CrashLoopBackOff · 3d4h · 12 restart(s)" || r.Detail != "last restart 5m ago" {
		t.Fatalf("row: %+v", r)
	}
}

func TestAHighButUnchangedRestartCountIsNotNews(t *testing.T) {
	// Up 200 days with 47 restarts is healthy. The growth is the signal.
	e := newEnv(t)
	e.Seed(t, "pod_restarts", map[string]int{"default/worker-1": 47})
	checktest.NoAlerts(t, pods(t, e, "default worker-1 1/1 Running 47 200d"))
}

func TestRestartGrowthSinceTheLastRunDoesAlert(t *testing.T) {
	e := newEnv(t)
	e.Seed(t, "pod_restarts", map[string]int{"default/worker-1": 43})
	s := pods(t, e, "default worker-1 1/1 Running 47 200d")
	alertCount(t, s, 1)
	if got := statusOf(t, s, "default/worker-1"); got != check.Caution {
		t.Fatalf("status %v", got)
	}
	if d := checktest.Rows(s)["default/worker-1"].Detail; d != "+4 restart(s) since last run" {
		t.Fatalf("detail %q", d)
	}
	var saved map[string]int
	if !e.Loaded(t, "pod_restarts", &saved) || saved["default/worker-1"] != 47 {
		t.Fatalf("saved %v", saved)
	}
}

func TestHealthyPodsAreACountNotAWall(t *testing.T) {
	var lines []string
	for i := range 60 {
		lines = append(lines, fmt.Sprintf("ns%d pod-%d 1/1 Running 0 5d", i, i))
	}
	s := pods(t, newEnv(t), lines...)
	for _, l := range checktest.Labels(s) {
		if strings.HasPrefix(l, "ns") {
			t.Fatalf("healthy pod listed: %q", l)
		}
	}
	if v := checktest.Rows(s)["Pods (cluster-wide)"].Value; !strings.Contains(v, "60 running") {
		t.Fatalf("summary %q", v)
	}
}

func TestProblemPodsAreCapped(t *testing.T) {
	var lines []string
	for i := range 25 {
		lines = append(lines, fmt.Sprintf("ns%d bad-%d 0/1 CrashLoopBackOff 3 5d", i, i))
	}
	s := pods(t, newEnv(t), lines...)
	n := 0
	for _, l := range checktest.Labels(s) {
		if strings.HasPrefix(l, "ns") {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("%d pod rows, want 10", n)
	}
	if !strings.Contains(checktest.Rows(s)["…"].Value, "15 more pod(s)") {
		t.Fatalf("rows: %q", checktest.Labels(s))
	}
	alertCount(t, s, 1)
	if !strings.HasSuffix(s.Alerts[0].Msg, "(+22 more)") {
		t.Fatalf("alert %q", s.Alerts[0].Msg)
	}
}

func TestMultiNodeClusterScopesPodsToThisHost(t *testing.T) {
	// auto scope: on a real cluster each node's report leads with its own
	// pods, so the same CrashLoopBackOff is not emailed once per node.
	e := newEnv(t)
	e.Fake.Expect("--field-selector=spec.nodeName=", "default mine-1 1/1 Running 0 5d")
	cluster(e, "k3s-01||v1.30.5|Ready=True,|k3s-01,10.0.0.5,\nk3s-02||v1.30.5|Ready=True,|k3s-02,10.0.0.6,")
	s := e.Run(Check{})
	if !e.Fake.Ran("--field-selector=spec.nodeName=k3s-01") {
		t.Fatalf("calls: %q", e.Fake.Calls())
	}
	if _, ok := checktest.Rows(s)["Pods (on this host)"]; !ok {
		t.Fatalf("rows: %q", checktest.Labels(s))
	}
}

func TestMaxPodsStopsReading(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxPods = 2 })
	s := pods(t, e, "a p1 1/1 Running 0 5d", "a p2 1/1 Running 0 5d", "a p3 0/1 CrashLoopBackOff 1 5d")
	checktest.NoAlerts(t, s)
	if v := checktest.Rows(s)["Pods (cluster-wide)"].Value; !strings.HasPrefix(v, "2 running") {
		t.Fatalf("summary %q", v)
	}
}

func TestFailedPodQueryIsInfo(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	e.Fake.Expect(podsCmd, "").Code(1).Stderr(`Error from server (Forbidden): pods is forbidden`)
	s := e.Run(Check{})
	if r := checktest.Rows(s)["Pods"]; r.Status != check.Info || !strings.Contains(r.Value, "Forbidden") {
		t.Fatalf("Pods row: %+v", r)
	}
	checktest.NoAlerts(t, s)
}
