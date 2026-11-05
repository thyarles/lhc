package hub

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/kube"
)

func TestTheMailDomainComesFromTheSMTPSettings(t *testing.T) {
	cases := []struct{ from, host, domain, source string }{
		{"lhc@example.com", "relay.example.org", "example.com", "smtp.from"},
		{"Health Check <lhc@Example.COM>", "", "example.com", "smtp.from"},
		{"", "relay.example.com", "example.com", "smtp.host"},
		{"", "smtp.mail.example.com.", "mail.example.com", "smtp.host"},
		{"", "192.0.2.10", "", ""},
		{"", "2001:db8::1", "", ""},
		{"", "relay", "", ""},
		{"", "example.com", "", ""}, // "com" is not a domain to mail at
		{"not an address", "relay.example.com", "", ""},
	}
	for _, c := range cases {
		d, s := MailDomain(c.from, c.host)
		if d != c.domain || s != c.source {
			t.Errorf("MailDomain(%q, %q) = %q, %q; want %q, %q", c.from, c.host, d, s, c.domain, c.source)
		}
	}
}

func TestOnlyNamesAndAddressesInTheDomainAreMailed(t *testing.T) {
	to, skipped := Recipients("name.surname, Other.Person@EXAMPLE.com,,  third_one+oom ", "example.com")
	if !slices.Equal(to, []string{"name.surname@example.com", "Other.Person@EXAMPLE.com", "third_one+oom@example.com"}) || skipped != nil {
		t.Fatalf("to %q, skipped %q", to, skipped)
	}
	bad := map[string]string{
		"name@example.org":             "not @example.com",
		"name@sub.example.com":         "not @example.com",
		"Name Surname <n@example.com>": "has a space",
		"name surname":                 "has a space",
		"name\nBcc: x@example.org":     "has a space or a line break",
		"name\r":                       "line break",
		"<name@example.com>":           "not @example.com",
		"a@b@example.com":              "not @example.com",
		".name":                        "not an address",
		"":                             "",
	}
	for e, why := range bad {
		to, skipped := Recipients(e, "example.com")
		if to != nil {
			t.Errorf("%q gave %q", e, to)
		}
		if why == "" {
			continue
		}
		if len(skipped) != 1 || !strings.Contains(skipped[0], why) || strings.ContainsAny(skipped[0], "\r\n") {
			t.Errorf("%q skipped as %q, want %q", e, skipped, why)
		}
	}
}

func TestAtMostFiveEntriesAndNoDuplicates(t *testing.T) {
	to, skipped := Recipients("a, b, A@example.com, c, d, e, f", "example.com")
	if !slices.Equal(to, []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}) {
		t.Fatalf("to %q", to)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0], `"e" skipped (only the first 5`) {
		t.Fatalf("skipped %q", skipped)
	}
}

const (
	api1UID = "11111111-0000-0000-0000-000000000001"
	api2UID = "11111111-0000-0000-0000-000000000002"
	webUID  = "22222222-0000-0000-0000-000000000001"
)

// ownerCluster is team-a's api (two pods, on two workers) and web.
func ownerCluster() map[string]kube.Pod {
	return map[string]kube.Pod{
		api1UID: {Namespace: "team-a", Name: "api-6b7c9d8f5-x2k4p", Node: "w1", Owner: "Deployment team-a/api", Containers: map[string]kube.Container{"app": {Limit: "512Mi"}}},
		api2UID: {Namespace: "team-a", Name: "api-6b7c9d8f5-q8w2e", Node: "w2", Owner: "Deployment team-a/api", Containers: map[string]kube.Container{"app": {Limit: "512Mi"}}},
		webUID:  {Namespace: "team-a", Name: "web-5f6d7c8b9-abcde", Node: "w1", Owner: "Deployment team-a/web", Containers: map[string]kube.Container{"nginx": {}}},
	}
}

func killed(uid, ns, name string, n int) check.PodRef {
	return check.PodRef{UID: uid, Namespace: ns, Name: name, Reason: "oom_killed", Count: n, Last: t0.Add(-time.Hour)}
}

func worker(host string, ago time.Duration, refs ...check.PodRef) Entry {
	e := entry(host, "caution", ago, "OOM Killer: 1 kill(s) today")
	e.Pods = refs
	return e
}

func lookup(workloads, namespaces map[string]string) OwnerLookup {
	return OwnerLookup{
		Pods: ownerCluster(), Workloads: workloads, Namespaces: namespaces,
		Annotation: "lhc/notify", Domain: "example.com", MinKills: 10, Now: t0,
	}
}

var hosts = []Entry{
	worker("w1.example.com", time.Hour, killed(api1UID, "team-a", "api-6b7c9d8f5-x2k4p", 6), killed(webUID, "team-a", "web-5f6d7c8b9-abcde", 3)),
	worker("w2.example.com", 2*time.Hour, killed(api2UID, "team-a", "api-6b7c9d8f5-q8w2e", 5)),
}

func TestKillsAreSummedAcrossHostsByWorkload(t *testing.T) {
	ws := PlanOwners(hosts, lookup(map[string]string{"Deployment team-a/api": "dev.one", "Deployment team-a/web": "dev.two"}, nil))
	if len(ws) != 1 {
		t.Fatalf("workloads %+v", ws)
	}
	w := ws[0]
	if w.Key != "Deployment team-a/api" || w.Kills != 11 || len(w.Pods) != 2 || w.Pods[0].Node != "w1" || w.Pods[0].Limit != "512Mi" || w.Pods[0].Container != "app" {
		t.Fatalf("api %+v", w)
	}
	if !slices.Equal(w.To, []string{"dev.one@example.com"}) || w.Source != "Deployment team-a/api" {
		t.Fatalf("to %q from %q", w.To, w.Source)
	}
}

func TestBelowTheThresholdOrOffNothingIsPlanned(t *testing.T) {
	l := lookup(map[string]string{"Deployment team-a/api": "dev.one"}, nil)
	l.MinKills = 12
	if ws := PlanOwners(hosts, l); ws != nil {
		t.Fatalf("below: %+v", ws)
	}
	l.MinKills = 0
	if ws := PlanOwners(hosts, l); ws != nil {
		t.Fatalf("off: %+v", ws)
	}
}

func TestAnOldReportDoesNotCountAgain(t *testing.T) {
	old := []Entry{hosts[0], worker("w2.example.com", 25*time.Hour, killed(api2UID, "team-a", "api-6b7c9d8f5-q8w2e", 5))}
	if ws := PlanOwners(old, lookup(map[string]string{}, nil)); ws != nil {
		t.Fatalf("yesterday's kills counted: %+v", ws)
	}
}

func TestTheWorkloadBeatsThePodAndThePodBeatsTheNamespace(t *testing.T) {
	pods := ownerCluster()
	p := pods[api1UID]
	p.Annotations = map[string]string{"lhc/notify": "from.pod"}
	pods[api1UID] = p
	ns := map[string]string{"team-a": "from.namespace"}
	cases := []struct {
		workloads map[string]string
		pods      map[string]kube.Pod
		to, from  string
	}{
		{map[string]string{"Deployment team-a/api": "from.deployment"}, pods, "from.deployment@example.com", "Deployment team-a/api"},
		{map[string]string{"Deployment team-a/api": ""}, pods, "from.pod@example.com", "pod team-a/api-6b7c9d8f5-x2k4p"},
		{map[string]string{}, ownerCluster(), "from.namespace@example.com", "namespace team-a"},
	}
	for _, c := range cases {
		l := lookup(c.workloads, ns)
		l.Pods = c.pods
		ws := PlanOwners(hosts, l)
		if len(ws) != 1 || !slices.Equal(ws[0].To, []string{c.to}) || ws[0].Source != c.from {
			t.Errorf("want %s from %s, got %+v", c.to, c.from, ws)
		}
	}
}

func TestTheFirstAnnotationWinsEvenWhenNothingInItCanBeMailed(t *testing.T) {
	ws := PlanOwners(hosts, lookup(map[string]string{"Deployment team-a/api": "dev@example.org"}, map[string]string{"team-a": "lead"}))
	if len(ws) != 1 || ws[0].To != nil || len(ws[0].Skipped) != 1 || ws[0].Source != "Deployment team-a/api" {
		t.Fatalf("%+v", ws)
	}
}

func TestAPodThatIsGoneIsMatchedToItsDeploymentByName(t *testing.T) {
	gone := []Entry{
		worker("w1.example.com", time.Hour, killed("33333333-0000-0000-0000-000000000001", "team-a", "api-6b7c9d8f5-zzzzz", 8)),
		worker("w2.example.com", time.Hour, killed(api2UID, "team-a", "api-6b7c9d8f5-q8w2e", 5)),
	}
	ws := PlanOwners(gone, lookup(map[string]string{"Deployment team-a/api": "dev.one"}, nil))
	if len(ws) != 1 || ws[0].Kills != 13 || ws[0].Pods[0].Node != "w1.example.com" || ws[0].Pods[0].Limit != "" {
		t.Fatalf("%+v", ws)
	}
	// The Deployment is gone too: the pod is named, and the namespace's
	// annotation applies.
	ws = PlanOwners(gone[:1], OwnerLookup{
		Pods: ownerCluster(), Workloads: map[string]string{}, Namespaces: map[string]string{"team-a": "lead"},
		Annotation: "lhc/notify", Domain: "example.com", MinKills: 5, Now: t0,
	})
	if len(ws) != 1 || ws[0].Key != "Pod team-a/api-6b7c9d8f5-zzzzz" || !slices.Equal(ws[0].To, []string{"lead@example.com"}) {
		t.Fatalf("%+v", ws)
	}
	// Known only by its UID: nothing to go by.
	uidOnly := []Entry{worker("w1.example.com", time.Hour, check.PodRef{UID: "44444444-0000-0000-0000-000000000001", Reason: "oom_killed", Count: 50})}
	if ws := PlanOwners(uidOnly, lookup(map[string]string{}, map[string]string{"team-a": "lead"})); ws != nil {
		t.Fatalf("%+v", ws)
	}
}

func TestADeploymentIsGuessedOnlyFromAReplicaSetPodName(t *testing.T) {
	for name, want := range map[string]string{
		"domicilio-horizon-98445df4d-qbf9h": "Deployment ns/domicilio-horizon",
		"api-6b7c9d8f5-x2k4p":               "Deployment ns/api",
		"db-0":                              "",
		"job-28812345-abcde":                "Deployment ns/job", // checked against the cluster's Deployments
		"web-x2k4p":                         "",
		"api-6B7C9D8F5-x2k4p":               "",
	} {
		if got := guessDeployment("ns", name); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestOwnersAreRemindedAfterThePeriod(t *testing.T) {
	st := OwnerState{Mailed: map[string]time.Time{}}
	week := 7 * 24 * time.Hour
	if !st.Due("Deployment team-a/api", t0, week) {
		t.Fatal("never mailed is not due")
	}
	st.Mailed["Deployment team-a/api"] = t0
	for _, c := range []struct {
		after time.Duration
		due   bool
	}{
		{24 * time.Hour, false},
		{6 * 24 * time.Hour, false},
		{week - 2*time.Hour, true}, // a digest sent at 09:00 a week after an 11:00 takeover
		{week, true},
	} {
		if got := st.Due("Deployment team-a/api", t0.Add(c.after), week); got != c.due {
			t.Errorf("after %s: due %v", c.after, got)
		}
	}
	st.Mailed["Deployment team-b/old"] = t0.Add(-31 * 24 * time.Hour)
	if !st.Forget(t0, week) || len(st.Mailed) != 1 {
		t.Fatalf("not forgotten: %v", st.Mailed)
	}
}

func TestTheStateSurvivesAReload(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if st := s.LoadOwners(); st.Mailed == nil || len(st.Mailed) != 0 {
		t.Fatalf("first: %+v", st)
	}
	if err := s.SaveOwners(OwnerState{Mailed: map[string]time.Time{"Deployment team-a/api": t0}}); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadOwners().Mailed["Deployment team-a/api"]; !got.Equal(t0) {
		t.Fatalf("got %v", got)
	}
	if entries, err := s.List(); err != nil || len(entries) != 0 {
		t.Fatalf("the state file is listed as a host: %+v %v", entries, err)
	}
}

func TestTheOwnerMailNamesOnlyItsOwnPods(t *testing.T) {
	ws := PlanOwners(hosts, lookup(map[string]string{"Deployment team-a/api": "dev.one"}, nil))
	subject, text := OwnerMail(ws[0], OwnerMailInfo{Annotation: "lhc/notify", Remind: 7 * 24 * time.Hour, Version: "0.14.0", Now: t0})
	if subject != "[lhc] OOM kills: team-a/api · 11 today" {
		t.Fatalf("subject %q", subject)
	}
	flat := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{"team-a/api had 11 OOM kills today", "annotation lhc/notify on Deployment team-a/api names you", "a reminder follows every 7 days"} {
		if !strings.Contains(flat, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	for _, want := range []string{
		"  team-a/api-6b7c9d8f5-x2k4p · container app · 6 kills · limit 512Mi · last 06:00 · node w1\n",
		"  team-a/api-6b7c9d8f5-q8w2e · container app · 5 kills · limit 512Mi · last 06:00 · node w2\n",
		"-- \nlhc 0.14.0\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	for _, not := range []string{"web", "example.com"} {
		if strings.Contains(text, not) {
			t.Errorf("%q in:\n%s", not, text)
		}
	}
}

func TestTheDigestSaysWhoWasTold(t *testing.T) {
	l := lookup(map[string]string{"Deployment team-a/api": "dev.one, dev.two"}, nil)
	l.MinKills = 3
	ws := PlanOwners(hosts, l)
	d, _ := BuildDigest(hosts, DigestState{}, DigestOptions{Self: "c1.example.com", SilentAfter: 26 * time.Hour, Now: t0, Cluster: ownerCluster(), Owners: ws})
	var lines []string
	for _, h := range d.NotOK {
		lines = append(lines, h.Details...)
	}
	want := []string{
		"OOM team-a/api-6b7c9d8f5-x2k4p · container app · 6 kills · limit 512Mi · Deployment team-a/api · told dev.one@example.com, dev.two@example.com",
		"OOM team-a/web-5f6d7c8b9-abcde · container nginx · 3 kills · limit none · Deployment team-a/web · no owner named",
		"OOM team-a/api-6b7c9d8f5-q8w2e · container app · 5 kills · limit 512Mi · Deployment team-a/api · told dev.one@example.com, dev.two@example.com",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
}
