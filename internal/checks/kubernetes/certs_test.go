package kubernetes

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

const (
	rke2TLS   = "/var/lib/rancher/rke2/server/tls/"
	rke2Agent = "/var/lib/rancher/rke2/agent/"
)

func in(days int) time.Time { return checktest.Now.Add(time.Duration(days)*24*time.Hour + time.Hour) }

// certs puts an rke2 control plane's files on the fake: leaves expiring in
// the given number of days, and a CA that expires tomorrow (never counted).
func certs(t *testing.T, e *checktest.Env, leafDays ...int) {
	t.Helper()
	e.Fake.File(rke2TLS+"server-ca.crt", checktest.CertPEM(t, "rke2-server-ca", in(1), true))
	names := []string{"client-admin.crt", "etcd/client.crt", "client-controller.crt"}
	for i, d := range leafDays {
		e.Fake.File(rke2TLS+names[i], checktest.CertPEM(t, names[i], in(d), false))
	}
}

func certRow(t *testing.T, s *check.Section) check.Row {
	t.Helper()
	r, ok := checktest.Rows(s)["Certificates"]
	if !ok {
		t.Fatalf("no Certificates row in %q", checktest.Labels(s))
	}
	return r
}

func TestCertificatesFarAwayIsQuiet(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	certs(t, e, 300, 200)
	s := e.Run(Check{})
	r := certRow(t, s)
	if r.Status != check.Info || r.Value != "first expiry 2026-07-24 (200d), 2 certs" || r.Detail != "client.crt" {
		t.Fatalf("got %v %q (%q)", r.Status, r.Value, r.Detail)
	}
	// One line in the text report, the date included.
	if len(r.Value) > 42 {
		t.Fatalf("%d columns: %q", len(r.Value), r.Value)
	}
	checktest.NoAlerts(t, s)
}

func TestCertificatesInRenewalWindowStayInfo(t *testing.T) {
	// What rke2 warns about hundreds of times a day: not news for lhc yet.
	e := newEnv(t)
	cluster(e, oneNode)
	certs(t, e, 108, 300)
	s := e.Run(Check{})
	r := certRow(t, s)
	if r.Status != check.Info || r.Value != "first expiry 2026-04-23 (108d), 2 certs" || r.Detail != "client-admin.crt, renewed on the next rke2-server restart" {
		t.Fatalf("got %v %q (%q)", r.Status, r.Value, r.Detail)
	}
	checktest.NoAlerts(t, s)
}

func TestCertificatesCAExpiryIsNotCounted(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	certs(t, e, 300)
	e.Fake.File(rke2Agent+"server-ca.crt", checktest.CertPEM(t, "ca", in(-3), true))
	s := e.Run(Check{})
	if r := certRow(t, s); r.Status != check.Info || r.Value != "first expiry 2026-11-01 (300d), 1 cert" {
		t.Fatalf("got %v %q", r.Status, r.Value)
	}
	checktest.NoAlerts(t, s)
}

func TestCertificatesCloseRaiseCaution(t *testing.T) {
	alertFor := func(days int) string {
		t.Helper()
		e := newEnv(t)
		cluster(e, oneNode)
		certs(t, e, days, 300)
		s := e.Run(Check{})
		if r := certRow(t, s); r.Status != check.Caution || r.Detail != "client-admin.crt, renewed on the next rke2-server restart" {
			t.Fatalf("%dd: got %v %q (%q)", days, r.Status, r.Value, r.Detail)
		}
		alertCount(t, s, 1)
		return s.Alerts[0].Msg
	}
	nine, eight := alertFor(9), alertFor(8)
	if nine != "rke2 certificates expire in 9d: restart rke2-server to renew them" {
		t.Fatalf("alert %q", nine)
	}
	noDigits := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return -1
			}
			return r
		}, s)
	}
	if noDigits(nine) != noDigits(eight) {
		t.Errorf("the alert identity must not change as the days go down: %q vs %q", nine, eight)
	}
}

func TestCertificatesThresholdIsConfigurable(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.CertificatesCautionDays = 0 })
	cluster(e, oneNode)
	certs(t, e, 2)
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestCertificatesExpiredIsUnhealthy(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	certs(t, e, -2, -1, 300)
	s := e.Run(Check{})
	r := certRow(t, s)
	if r.Status != check.Unhealthy || r.Value != "2 of 3 EXPIRED: client-admin.crt, client.crt" {
		t.Fatalf("got %v %q", r.Status, r.Value)
	}
	alertCount(t, s, 1)
	if s.Alerts[0].Status != check.Unhealthy || s.Alerts[0].Msg != "rke2 certificates EXPIRED: client-admin.crt, client.crt" {
		t.Fatalf("alert %+v", s.Alerts[0])
	}
}

func TestCertificatesOff(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Certificates = false })
	cluster(e, oneNode)
	certs(t, e, -2)
	s := e.Run(Check{})
	if _, ok := checktest.Rows(s)["Certificates"]; ok {
		t.Fatal("certificates: false must not read them")
	}
	checktest.NoAlerts(t, s)
}

func TestCertificatesNoneNoRow(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	if _, ok := checktest.Rows(e.Run(Check{}))["Certificates"]; ok {
		t.Fatal("no rke2/k3s files: no row")
	}
}

func TestCertificatesOnWorkerWithoutKubeconfig(t *testing.T) {
	e := newEnv(t)
	e.Fake.Expect("pgrep -x kubelet", "4242")
	e.Fake.File(rke2Agent+"client-kubelet.crt", checktest.CertPEM(t, "system:node", in(5), false))
	s := e.Run(Check{})
	if r := certRow(t, s); r.Status != check.Caution {
		t.Fatalf("got %v %q", r.Status, r.Value)
	}
	if _, ok := checktest.Rows(s)["Node Role"]; !ok {
		t.Fatalf("worker row missing: %q", checktest.Labels(s))
	}
	if !checktest.HasAlertContaining(s, "restart rke2-agent") {
		t.Fatalf("a worker restarts rke2-agent: %q", s.AlertMsgs())
	}
}

func TestCertificatesWhenStopped(t *testing.T) {
	// No kubelet, no kubeconfig, no kubectl: rke2 is installed but down. The
	// section stays applicable so the certificate row is not hidden.
	e := newEnv(t)
	e.Fake.File(rke2Agent+"client-kubelet.crt", checktest.CertPEM(t, "system:node", in(300), false))
	s := e.Run(Check{})
	if !s.Applicable {
		t.Fatal("section must stay applicable")
	}
	certRow(t, s)
	if r := checktest.Rows(s)["Node Role"]; !strings.Contains(r.Value, "installed") {
		t.Fatalf("got %q", r.Value)
	}
}

func TestCertificatesWhileAPIIsDown(t *testing.T) {
	e := newEnv(t)
	found(e)
	e.Fake.Expect(nodesCmd, "").Code(1).Stderr("error: You must be logged in to the server (the server has asked for the client to provide credentials)")
	certs(t, e, -1)
	s := e.Run(Check{})
	if r := certRow(t, s); r.Status != check.Unhealthy {
		t.Fatalf("got %v %q", r.Status, r.Value)
	}
	if !checktest.HasAlertContaining(s, "Kubernetes API not usable") {
		t.Fatalf("API alert missing: %q", s.AlertMsgs())
	}
}

func TestCertificatesCautionDaysValidation(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	c.CertificatesCautionDays = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "certificates_caution_days") {
		t.Fatalf("got %v", err)
	}
}
