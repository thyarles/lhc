//go:build e2e

// Package e2e drives the real lhc binary: fake commands on PATH, a fake SMTP
// relay in this process, and assertions on what reached the relay and what
// was written to the state directory.
//
//	go test -tags e2e ./test/e2e/...
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/config"
	"github.com/thyarles/lhc/internal/notify/smtp/smtptest"

	_ "github.com/thyarles/lhc/internal/checks/all"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lhc-e2e-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "lhc")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/lhc")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakes are shell scripts put first on the runner's PATH.
var fakes = map[string]string{
	// One filesystem at 97%: an UNHEALTHY condition and an alert.
	"df": `cat <<'EOF'
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/sda1         10000000   9700000    300000      97% /
EOF`,
	"nproc":  `echo 4`,
	"uname":  `case "$1" in -r) echo 6.1.0-test;; -m) echo x86_64;; esac`,
	"lscpu":  `echo "Model name: Fake CPU"`,
	"free":   "printf '              total        used        free\\nMem:     8000000000  2000000000  6000000000\\nSwap:             0           0           0\\n'",
	"mpstat": `exit 1`,
}

type host struct {
	t      *testing.T
	dir    string
	cfg    string
	state  string
	relay  *smtptest.Server
	stdout string
	stderr string
}

func newHost(t *testing.T, relay *smtptest.Server) *host {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := &host{t: t, dir: dir, cfg: filepath.Join(dir, "config.yaml"), state: filepath.Join(dir, "state"), relay: relay}

	// Only the checks the fakes make deterministic; everything else would
	// read the machine running the test.
	sets := []string{
		"hostname=e2e.example.com",
		"smtp.host=127.0.0.1",
		fmt.Sprintf("smtp.port=%d", relay.Port()),
		"email.daily_recipients=ops@example.com",
		"email.alert_recipients=team@example.com",
		"paths.state_dir=" + h.state,
		"paths.log_file=" + filepath.Join(dir, "lhc.log"),
	}
	for _, name := range config.Known() {
		if name != "system" && name != "disk" && name != "memory" {
			sets = append(sets, "checks."+name+".enabled=false")
		}
	}
	h.run(0, append([]string{"config", "set"}, sets...)...)
	return h
}

func (h *host) run(want int, args ...string) {
	h.t.Helper()
	cmd := exec.Command(binary, append(args, "--config", h.cfg)...)
	cmd.Env = append(os.Environ(),
		"LHC_PATH_OVERRIDE="+filepath.Join(h.dir, "bin"),
		"HOME="+h.dir, "KUBECONFIG=")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	h.stdout, h.stderr = out.String(), errb.String()
	got := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		got = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	if got != want {
		h.t.Fatalf("lhc %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, want, h.stdout, h.stderr)
	}
}

func (h *host) alertsState() []byte {
	b, err := os.ReadFile(filepath.Join(h.state, "alerts.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func relay(t *testing.T, opts ...func(*smtptest.Server)) *smtptest.Server {
	t.Helper()
	s, err := smtptest.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestANewUnhealthyFindingAlertsEveryoneOnceAndIsCommitted(t *testing.T) {
	r := relay(t)
	h := newHost(t, r)

	h.run(0, "run", "--format", "json")
	var rep struct {
		Schema  int    `json:"schema"`
		Host    string `json:"host"`
		Overall string `json:"overall"`
		Triage  struct {
			NotifyAll bool     `json:"notify_all"`
			New       []string `json:"new"`
		} `json:"triage"`
	}
	if err := json.Unmarshal([]byte(h.stdout), &rep); err != nil {
		t.Fatalf("stdout is not the JSON report: %v\n%s", err, h.stdout)
	}
	if rep.Schema != 1 || rep.Host != "e2e.example.com" || rep.Overall != "unhealthy" || !rep.Triage.NotifyAll {
		t.Fatalf("%+v", rep)
	}
	msgs := r.Messages()
	if len(msgs) != 1 || len(msgs[0].To) != 2 {
		t.Fatalf("want one message to both lists, got %+v", msgs)
	}
	if !strings.Contains(msgs[0].Data, "Subject: =?utf-8?q?[ACTION]") {
		t.Fatalf("not an [ACTION] mail:\n%.600s", msgs[0].Data)
	}
	if h.alertsState() == nil {
		t.Fatal("delivered, but the alert was not committed")
	}
	reports, _ := filepath.Glob(filepath.Join(h.state, "reports", "health-e2e.example.com-*.html"))
	if len(reports) != 1 {
		t.Fatalf("saved reports: %v", reports)
	}

	// Same condition next run: a heartbeat to the daily list only.
	h.run(0, "run")
	msgs = r.Messages()
	if len(msgs) != 2 || len(msgs[1].To) != 1 || msgs[1].To[0] != "ops@example.com" {
		t.Fatalf("second run should reach only the daily list: %+v", msgs[1:])
	}
}

func TestAFailedDeliveryLeavesTheAlertUnacknowledged(t *testing.T) {
	r := relay(t, func(s *smtptest.Server) { s.RejectData = true })
	h := newHost(t, r)

	h.run(2, "run")
	if h.alertsState() != nil {
		t.Fatal("the relay refused the mail, yet the alert was marked as notified")
	}
	if !strings.Contains(h.stderr+readFile(t, filepath.Join(h.dir, "lhc.log")), "will retry next run") {
		t.Fatalf("no retry notice:\n%s", h.stderr)
	}
}

func TestReportChangesNoState(t *testing.T) {
	h := newHost(t, relay(t))
	h.run(0, "report", "--format", "json")
	if entries, err := os.ReadDir(h.state); err == nil && len(entries) > 0 {
		t.Fatalf("lhc report wrote state: %v", entries)
	}
	h.run(0, "run", "--dry-run")
	if entries, err := os.ReadDir(h.state); err == nil && len(entries) > 0 {
		t.Fatalf("lhc run --dry-run wrote state: %v", entries)
	}
}

func TestConfigValidateRejectsATypo(t *testing.T) {
	h := newHost(t, relay(t))
	if err := os.WriteFile(h.cfg, []byte("smtp:\n  hots: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run(1, "config", "validate")
	if !strings.Contains(h.stderr, "hots") {
		t.Fatalf("error does not name the bad key: %s", h.stderr)
	}
}

func readFile(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
