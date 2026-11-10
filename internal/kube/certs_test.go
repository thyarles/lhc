package kube

import (
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/checktest"
)

var certNow = time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)

func days(n int) time.Time { return certNow.Add(time.Duration(n) * 24 * time.Hour) }

func TestReadCertsRKE2Server(t *testing.T) {
	const root = "/var/lib/rancher/rke2"
	r := checktest.NewRunner().
		File(root+"/server/tls/client-admin.crt", checktest.CertPEM(t, "system:admin", days(200), false)).
		File(root+"/server/tls/client-ca.crt", checktest.CertPEM(t, "rke2-client-ca", days(3), true)).
		File(root+"/server/tls/client-ca.key", "not a certificate").
		File(root+"/server/tls/etcd/client.crt", checktest.CertPEM(t, "etcd-client", days(108), false)).
		File(root+"/server/tls/etcd/server-ca.crt", checktest.CertPEM(t, "etcd-server-ca", days(1), true)).
		File(root+"/agent/client-kubelet.crt", checktest.CertPEM(t, "system:node", days(150), false)).
		File(root+"/agent/serving-kubelet.crt", checktest.CertPEM(t, "node", days(140), false)+checktest.CertPEM(t, "ca", days(2), true)).
		File(root+"/agent/broken.crt", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n").
		// agent/ subdirectories are containerd state and images: never walked.
		File(root+"/agent/etc/containerd/old.crt", checktest.CertPEM(t, "ignored", days(-5), false))

	c := ReadCerts(r)
	if c.Distro != "rke2" || !c.Server || c.Service() != "rke2-server" {
		t.Fatalf("distro %q server %v service %q", c.Distro, c.Server, c.Service())
	}
	if len(c.Leaves) != 4 {
		t.Fatalf("want 4 leaves (CAs, keys and agent subdirectories skipped), got %+v", c.Leaves)
	}
	if c.Leaves[0].File != "server/tls/etcd/client.crt" || c.Leaves[0].Base() != "client.crt" {
		t.Errorf("soonest first: got %+v", c.Leaves[0])
	}
	if c.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", c.Unreadable)
	}
	if got := c.Expired(certNow); len(got) != 0 {
		t.Errorf("nothing is expired, got %+v", got)
	}
	if got := c.Expired(days(110)); len(got) != 1 {
		t.Errorf("one expired at +110d, got %+v", got)
	}
}

func TestReadCertsAgentsAndK3s(t *testing.T) {
	cases := []struct {
		name, file, distro, service string
	}{
		{"rke2 agent", "/var/lib/rancher/rke2/agent/client-kubelet.crt", "rke2", "rke2-agent"},
		{"k3s server", "/var/lib/rancher/k3s/server/tls/client-admin.crt", "k3s", "k3s"},
		{"k3s agent", "/var/lib/rancher/k3s/agent/client-kubelet.crt", "k3s", "k3s-agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ReadCerts(checktest.NewRunner().File(tc.file, checktest.CertPEM(t, "x", days(30), false)))
			if !c.Found() || c.Distro != tc.distro || c.Service() != tc.service || len(c.Leaves) != 1 {
				t.Fatalf("got %+v service %q", c, c.Service())
			}
		})
	}
}

func TestReadCertsNothing(t *testing.T) {
	if c := ReadCerts(checktest.NewRunner()); c.Found() {
		t.Fatalf("no rke2/k3s: want nothing, got %+v", c)
	}
	// Only CA certificates (or none at all) under the directories: not found.
	r := checktest.NewRunner().
		File("/var/lib/rancher/rke2/server/tls/server-ca.crt", checktest.CertPEM(t, "ca", days(5), true)).
		Dir("/var/lib/rancher/rke2/agent")
	if c := ReadCerts(r); c.Found() {
		t.Fatalf("CAs only: want nothing, got %+v", c)
	}
}

func TestReadCertsUnreadableFile(t *testing.T) {
	r := checktest.NewRunner().Unreadable("/var/lib/rancher/rke2/agent/client-kubelet.crt")
	c := ReadCerts(r)
	if !c.Found() || c.Unreadable != 1 || len(c.Leaves) != 0 {
		t.Fatalf("got %+v", c)
	}
}
