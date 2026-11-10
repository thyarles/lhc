//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/checktest"
)

// certNode is an rke2 control-plane node whose first certificate expires
// in ten days, with cert_restart on. Its fake systemctl reports rke2-server
// active, and a restart records itself and puts a renewed certificate in
// place, as rke2 does on start.
func certNode(t *testing.T, name, key string, collectors []string) *host {
	t.Helper()
	h := hubHost(t, name, key, collectors...)
	h.run(0, "config", "set", "cert_restart.before=30d")
	rancher := filepath.Join(h.dir, "rancher")
	tls := filepath.Join(rancher, "rke2", "server", "tls")
	if err := os.MkdirAll(tls, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(tls, "client-admin.crt"), checktest.CertPEM(t, "system:admin", now.Add(10*24*time.Hour), false))
	write(filepath.Join(tls, "server-ca.crt"), checktest.CertPEM(t, "rke2-server-ca", now.Add(2*24*time.Hour), true))
	write(filepath.Join(h.dir, "renewed.crt"), checktest.CertPEM(t, "system:admin", now.Add(365*24*time.Hour), false))
	systemctl := fmt.Sprintf(`case "$1" in
is-active) if [ "$2" = rke2-server ]; then echo active; else echo inactive; exit 3; fi ;;
restart) echo "$2" >> %[1]s/restarts; cp %[1]s/renewed.crt %[2]s/client-admin.crt ;;
esac`, h.dir, tls)
	if err := os.WriteFile(filepath.Join(h.dir, "bin", "systemctl"), []byte("#!/bin/sh\n"+systemctl+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env = append(h.env, "LHC_RANCHER_DIR="+rancher)
	return h
}

func (h *host) restarts() string {
	b, err := os.ReadFile(filepath.Join(h.dir, "restarts"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestCertRestartOneNodeOfAGroupANight(t *testing.T) {
	first := newHost(t, relay(t))
	key := keygen(t, first)
	var cols []*collector
	var urls []string
	for i, h := range []*host{first, newHost(t, relay(t)), newHost(t, relay(t))} {
		h.run(0, "config", "set", fmt.Sprintf("hostname=c%d.example.com", i+1))
		c := startCollector(t, h, key)
		cols, urls = append(cols, c), append(urls, c.url)
	}
	n1 := certNode(t, "k8s-node01.example.com", key, urls)
	n2 := certNode(t, "k8s-node02.example.com", key, urls)

	// A dry run asks, but neither records nor restarts anything.
	n1.run(0, "cert-restart", "--dry-run")
	if !strings.Contains(n1.stderr, "3 of 3 collectors would grant: would restart rke2-server") || n1.restarts() != "" {
		t.Fatalf("dry run:\n%s", n1.stderr)
	}

	n1.run(0, "cert-restart", "--scheduled")
	for _, want := range []string{
		"due: first certificate (client-admin.crt) expires in 9d",
		"3 of 3 collectors granted (group k8s-node): restarting rke2-server",
		"renewed: first certificate now expires in 364d",
		"Hub: 3 of 3 collector(s) accepted the report",
	} {
		if !strings.Contains(n1.stderr+n1.stdout, want) {
			t.Errorf("node01 output lacks %q:\n%s\n%s", want, n1.stderr, n1.stdout)
		}
	}
	if got := n1.restarts(); got != "rke2-server\n" {
		t.Fatalf("node01 restarts %q", got)
	}

	// The same night, the other node of the group is refused everywhere.
	n2.run(0, "cert-restart", "--scheduled")
	if !strings.Contains(n2.stderr, "refused: k8s-node01.example.com restarts today") || !strings.Contains(n2.stderr, "0 of 3 collectors granted, 2 needed") {
		t.Fatalf("node02:\n%s", n2.stderr)
	}
	if got := n2.restarts(); got != "" {
		t.Fatalf("node02 restarted: %q", got)
	}

	// The report after the restart tells the collectors about it.
	for _, c := range cols {
		rep := c.stored(t, "k8s-node01.example.com")
		cr, _ := rep["cert_restart"].(map[string]any)
		if cr == nil || cr["group"] != "k8s-node" || cr["last_renewed"] != true || cr["service"] != "rke2-server" {
			t.Fatalf("%s stored %v", c.url, rep["cert_restart"])
		}
		if !strings.Contains(c.log.String(), "claim cert-restart from k8s-node02.example.com (group k8s-node): refused") {
			t.Fatalf("collector log:\n%s", c.log)
		}
	}

	// The digest shows the group: node01 restarted and renewed.
	cols[0].run(0, "hub", "digest", "--dry-run")
	for _, want := range []string{"CERTIFICATE RESTARTS", "k8s-node01.example.com restarted rke2-server at", "renewed", "(first expires in 364d)"} {
		if !strings.Contains(cols[0].stdout, want) {
			t.Fatalf("digest lacks %q:\n%s", want, cols[0].stdout)
		}
	}
}
