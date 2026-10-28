//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Hosts installed on v0.6/v0.7 kept that day's zombie defaults (10/50) in
// their files after the defaults moved to 2500/3000. The upgrade's install
// lets them follow the new defaults and keeps what someone chose (#46).
func TestInstallLetsPinnedOldDefaultsFollowTheNewOnes(t *testing.T) {
	h := newHost(t, relay(t))
	// The user crontab, faked: install --backend cron reads and writes it.
	tab := filepath.Join(h.dir, "crontab")
	fake := "#!/bin/sh\ncase \"$1\" in -l) cat " + tab + " 2>/dev/null ;; -r) rm -f " + tab + " ;; *) cp \"$1\" " + tab + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(h.dir, "bin", "crontab"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// What an older install wrote: every default of its day, active.
	src := readFile(t, h.cfg)
	for old, pinned := range map[string]string{
		"    #zombie_caution: 2500":   "    zombie_caution: 10",
		"    #zombie_unhealthy: 3000": "    zombie_unhealthy: 50",
		"  #tls: none":                "  tls: none",
	} {
		if !strings.Contains(src, old+"\n") {
			t.Fatalf("no %q in:\n%s", old, src)
		}
		src = strings.Replace(src, old+"\n", pinned+"\n", 1)
	}
	if err := os.WriteFile(h.cfg, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	h.run(0, "install", "--backend", "cron")
	out := h.stdout + h.stderr
	for _, want := range []string{
		"checks.processes.zombie_caution: 10 was the old default, now follows the default (2500)",
		"checks.processes.zombie_unhealthy: 50 was the old default, now follows the default (3000)",
		"1 setting(s) at their default now follow it",
		"crontab entry:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	if readFile(t, h.cfg+".bak") != src {
		t.Fatal("no backup of the old file")
	}
	h.run(0, "config", "show")
	for _, want := range []string{"checks.processes.zombie_caution = 2500", "checks.processes.zombie_unhealthy = 3000", "smtp.host = 127.0.0.1"} {
		if !strings.Contains(h.stdout, want) {
			t.Fatalf("no %q in:\n%s", want, h.stdout)
		}
	}

	// The next upgrade finds nothing to change.
	h.run(0, "install", "--backend", "cron")
	if out := h.stdout + h.stderr; strings.Contains(out, "default") {
		t.Fatalf("a second migration:\n%s", out)
	}
}
