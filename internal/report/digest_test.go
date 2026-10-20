package report

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	tw "github.com/thyarles/lhc/internal/textwrap"
)

var digestNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.FixedZone("-03", -3*3600))

func goldenDigest() *Digest {
	return &Digest{
		Collector: "lhc-c1.example.com", Version: "1.2.3", Generated: digestNow, SilentAfter: 26 * time.Hour,
		Overall: check.Unhealthy,
		Collectors: []DigestPeer{
			{Host: "lhc-c1.example.com", Self: true, Last: digestNow.Add(-6 * time.Hour)},
			{Host: "lhc-c2.example.com", Last: digestNow.Add(-5 * time.Hour)},
			{Host: "lhc-c3.example.com", Last: digestNow.Add(-50 * time.Hour), Silent: true},
		},
		Silent: []DigestHost{
			{Host: "lhc-c3.example.com", Status: check.Unhealthy, Last: digestNow.Add(-50 * time.Hour)},
		},
		NotOK: []DigestHost{
			{Host: "db01.example.com", Status: check.Unhealthy, New: true, Alerts: []string{"Disk / at 97%", "Service postgresql.service failed"}, More: 1},
			{Host: "web01.example.com", Status: check.Caution, Alerts: []string{"Reboot pending for more than 14 days"}},
		},
		NewHosts:  []string{"web02.example.com"},
		Total:     40,
		OK:        36,
		UptimeMin: 3 * time.Hour, UptimeMax: 1320 * 24 * time.Hour,
		Updates: 1400, Reboot: 3,
	}
}

func digestHTML(t *testing.T, d *Digest) string {
	t.Helper()
	out, err := DigestHTML(d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGoldenDigestText(t *testing.T) { golden(t, "digest.txt", DigestText(goldenDigest())) }

func TestGoldenDigestHTML(t *testing.T) { golden(t, "digest.html", digestHTML(t, goldenDigest())) }

func TestTheDigestFitsTheWidthWithFortyHostsAndLongNames(t *testing.T) {
	d := goldenDigest()
	d.Collector = "a-collector-with-a-very-long-name.datacenter-one.example.com"
	d.NotOK = nil
	for i := range 40 {
		d.NotOK = append(d.NotOK, DigestHost{
			Host:   fmt.Sprintf("k8s-worker-node-%02d.a-rather-long-subdomain.example.com", i),
			Status: check.Caution, New: i%2 == 0,
			Alerts: []string{"Pod kube-system/some-deployment-name-7d9f8b6c5-abcde in CrashLoopBackOff for a long while"},
		})
	}
	out := DigestText(d)
	for i, l := range strings.Split(out, "\n") {
		if tw.Len(l) > W {
			t.Errorf("line %d is %d columns: %q", i+1, tw.Len(l), l)
		}
	}
	if !strings.Contains(out, "k8s-worker-node-39.a-rather-long-subdomain.example.com") {
		t.Fatal("a long host name was cut")
	}
}

func TestTheDigestHTMLFollowsTheReportsRules(t *testing.T) {
	out := digestHTML(t, goldenDigest())
	if !strings.Contains(out, `content="only light"`) || strings.Contains(out, "prefers-color-scheme") {
		t.Fatal("not light-only")
	}
	if naked := nakedColours(out); len(naked) > 0 {
		t.Fatalf("these would vanish on a force-inverting client: %q", naked)
	}
}

func TestTheDigestHTMLEscapesWhatHostsSend(t *testing.T) {
	d := goldenDigest()
	d.NotOK[0].Alerts = []string{`<script>alert(1)</script>`}
	out := digestHTML(t, d)
	if strings.Contains(out, "<script>") || !strings.Contains(out, "&lt;script&gt;") {
		t.Fatal("an alert message reached the HTML unescaped")
	}
}

func TestAnAllGoodDigestSaysSoInOneLine(t *testing.T) {
	d := &Digest{
		Collector: "c1.example.com", Generated: digestNow, SilentAfter: 26 * time.Hour, Overall: check.OK,
		Collectors: []DigestPeer{{Host: "c1.example.com", Self: true}}, Total: 3, OK: 3,
	}
	out := DigestText(d)
	for _, absent := range []string{"  SILENT —", "  NOT OK", "  NEW HOSTS"} {
		if strings.Contains(out, absent) {
			t.Fatalf("an empty %s heading:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "  3 hosts OK\n") || DigestSubject(d) != "[hub] 3 hosts · 3 OK · via c1.example.com" {
		t.Fatalf("subject %q\n%s", DigestSubject(d), out)
	}
}
