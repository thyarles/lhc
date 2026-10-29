package hub

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/kube"
	"github.com/thyarles/lhc/internal/report"
)

const (
	apiUID  = "1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	goneUID = "9f8e7d6c-5b4a-3928-1706-f5e4d3c2b1a0"
)

// oomHost is a worker whose report names OOM-killed pods.
func oomHost(refs ...check.PodRef) Entry {
	e := entry("w1.example.com", "caution", time.Hour, "OOM Killer: 5 kill(s) today")
	e.Pods = refs
	return e
}

var (
	apiRef  = check.PodRef{UID: apiUID, Namespace: "team-a", Name: "api-6b7c9d-x2k4p", Container: "app", Process: "java", Reason: "oom_killed", Count: 3}
	goneRef = check.PodRef{UID: goneUID, Process: "python3", Reason: "oom_killed", Count: 2}
)

var cluster = map[string]kube.Pod{
	apiUID: {Namespace: "team-a", Name: "api-6b7c9d-x2k4p", Owner: "Deployment team-a/api", Containers: map[string]kube.Container{
		"app": {Limit: "512Mi"}, "sidecar": {},
	}},
	"00000000-0000-0000-0000-000000000002": {Namespace: "team-b", Name: "batch-x", Containers: map[string]kube.Container{"worker": {}}},
}

func details(t *testing.T, e Entry, o DigestOptions) []string {
	t.Helper()
	o.Self, o.SilentAfter, o.Now = "c1.example.com", 26*time.Hour, t0
	d, _ := BuildDigest([]Entry{e}, DigestState{}, o)
	if len(d.NotOK) != 1 {
		t.Fatalf("not ok: %+v", d.NotOK)
	}
	return d.NotOK[0].Details
}

func TestTheClusterAddsTheLimitAndTheOwner(t *testing.T) {
	got := details(t, oomHost(goneRef, apiRef), DigestOptions{Cluster: cluster})
	want := []string{
		"OOM team-a/api-6b7c9d-x2k4p · container app · 3 kills · limit 512Mi · Deployment team-a/api",
		"OOM pod UID 9f8e7d6c-… · 2 kills · pod no longer exists",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("details:\n%s", strings.Join(got, "\n"))
	}
}

func TestAPodTheHostCouldNotResolveIsFoundByUID(t *testing.T) {
	ref := check.PodRef{UID: "00000000-0000-0000-0000-000000000002", Process: "worker", Reason: "oom_killed", Count: 1}
	got := details(t, oomHost(ref), DigestOptions{Cluster: cluster})
	// Its only container is the one; no limit is set.
	if !slices.Equal(got, []string{"OOM team-b/batch-x · container worker · 1 kill · limit none"}) {
		t.Fatalf("details: %q", got)
	}
}

func TestWithoutTheLookupTheLinesShowWhatTheHostSent(t *testing.T) {
	got := details(t, oomHost(apiRef, goneRef), DigestOptions{})
	want := []string{
		"OOM team-a/api-6b7c9d-x2k4p · container app · java · 3 kills · container limit",
		"OOM pod UID 9f8e7d6c-… · python3 · 2 kills · container limit",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("details:\n%s", strings.Join(got, "\n"))
	}
}

func TestAtMostThreeOOMLinesPerHost(t *testing.T) {
	var refs []check.PodRef
	for i := range 5 {
		refs = append(refs, check.PodRef{UID: apiUID[:35] + string(rune('a'+i)), Name: "p", Namespace: "ns", Reason: "oom_killed", Count: 5 - i})
	}
	got := details(t, oomHost(refs...), DigestOptions{})
	if len(got) != report.DetailsListed || got[2] != "(+3 more OOM-killed pods)" || !strings.Contains(got[0], "5 kills") {
		t.Fatalf("details: %q", got)
	}
}

func TestAFailedLookupIsAFooterNotAnAlert(t *testing.T) {
	e := oomHost(apiRef)
	d, _ := BuildDigest([]Entry{e}, DigestState{}, DigestOptions{
		Self: "c1.example.com", SilentAfter: 26 * time.Hour, Now: t0,
		LookupErr: "The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?",
	})
	if d.Overall != check.Caution || len(d.NotOK[0].Alerts) != 1 {
		t.Fatalf("digest: %+v", d)
	}
	text := strings.Join(strings.Fields(report.DigestText(d)), " ")
	if !strings.Contains(text, "Kubernetes lookup failed: The connection to the server 127.0.0.1:6443 was refused - did you") || !strings.Contains(text, "- OOM Killer: 5 kill(s) today OOM team-a/api-6b7c9d-x2k4p · container app") {
		t.Fatalf("text:\n%s", text)
	}
	html, err := report.DigestHTML(d)
	if err != nil || !strings.Contains(html, "Kubernetes lookup failed") || !strings.Contains(html, "OOM team-a/api-6b7c9d-x2k4p") {
		t.Fatalf("html: %v", err)
	}
}
