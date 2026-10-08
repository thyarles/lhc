package logs

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

// logsEnv is a host with journalctl, a syslog and an auth log, whose log
// greps return count lines for each pattern containing a key of counts.
func logsEnv(t *testing.T, cfg *Config, counts map[string]int) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	e.Fake.Tool("journalctl")
	e.Fake.File("/var/log/syslog", "").File("/var/log/auth.log", "")
	for key, n := range counts {
		e.Fake.Expect(key, strings.Repeat("sample "+key+"\n", n))
	}
	return e
}

func statusOf(t *testing.T, s *check.Section, label string) check.Status {
	t.Helper()
	r, ok := checktest.Rows(s)[label]
	if !ok {
		t.Fatalf("no row labelled %q in %q", label, checktest.Labels(s))
	}
	return r.Status
}

func TestASingleSegfaultIsNotAnIncident(t *testing.T) {
	s := logsEnv(t, nil, map[string]int{"segfault": 1}).Run(Check{})
	checktest.NoAlerts(t, s)
	if got := statusOf(t, s, "Segmentation Faults"); got != check.Info {
		t.Fatalf("status %v", got)
	}
}

func TestAPileOfSegfaultsEscalates(t *testing.T) {
	s := logsEnv(t, nil, map[string]int{"segfault": 12}).Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"Segmentation Faults: 12 occurrence(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestASingleKernelPanicEscalates(t *testing.T) {
	s := logsEnv(t, nil, map[string]int{"kernel panic": 1}).Run(Check{})
	if got := statusOf(t, s, "Kernel Panic"); got != check.Unhealthy {
		t.Fatalf("status %v", got)
	}
}

func TestBackgroundSSHProbingDoesNotAlert(t *testing.T) {
	// Any internet-facing host sees a constant level of these.
	s := logsEnv(t, nil, map[string]int{"BREAK-IN ATTEMPT": 20}).Run(Check{})
	checktest.NoAlerts(t, s)
}

func TestSSHProbingAboveTheThresholdAlerts(t *testing.T) {
	s := logsEnv(t, nil, map[string]int{"BREAK-IN ATTEMPT": 200}).Run(Check{})
	if len(s.Alerts) != 1 {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
}

func TestQuietLogsAreAllOK(t *testing.T) {
	s := logsEnv(t, nil, nil).Run(Check{})
	if s.Status != check.OK {
		t.Fatalf("status %v", s.Status)
	}
	checktest.NoAlerts(t, s)
	if len(s.Rows) != 9 {
		t.Fatalf("rows: %q", checktest.Labels(s))
	}
}

func TestSSHBruteForceMinIsConfigurable(t *testing.T) {
	cfg := &Config{Toggle: check.On, SSHBruteForceMin: 10}
	s := logsEnv(t, cfg, map[string]int{"Invalid user": 20}).Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"SSH Brute Force: 20 occurrence(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
	if d := checktest.Rows(s)["SSH Brute Force"].Detail; d != "escalates at ≥ 10 today" {
		t.Fatalf("detail %q", d)
	}
}

func TestEscalationShowsTheRecentEntries(t *testing.T) {
	e := logsEnv(t, nil, nil)
	e.Fake.Expect("oom.kill", "Jan  5 03:00:01 host kernel: Out of memory: Killed process 1 (java)\n"+
		"Jan  5 04:00:01 host kernel: Out of memory: Killed process 2 ("+strings.Repeat("x", 200)+")")
	s := e.Run(Check{})
	labels := checktest.Labels(s)
	i := slices.Index(labels, "Recent entries")
	if i != 1 || labels[i+1] != "" || labels[i+2] != "" {
		t.Fatalf("labels: %q", labels)
	}
	if r := s.Rows[i+2]; r.Status != check.Caution || len([]rune(r.Value)) != 120 {
		t.Fatalf("sample row: %+v", r)
	}
}

func TestInformationalPatternsNeverEscalate(t *testing.T) {
	s := logsEnv(t, nil, map[string]int{"New USB device found": 30, "sudo.*COMMAND": 100}).Run(Check{})
	checktest.NoAlerts(t, s)
	for _, l := range []string{"USB Device Added", "sudo Escalation"} {
		if r := checktest.Rows(s)[l]; r.Status != check.Info || r.Detail != "" {
			t.Errorf("%s: %+v", l, r)
		}
	}
}

func TestJournaldOnlyHostIsStillScanned(t *testing.T) {
	// No /var/log/syslog (Debian 12 without rsyslog): the journal holds it all.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("journalctl")
	e.Fake.Expect("kernel panic", "Jan  5 03:00:01 host kernel: Kernel panic - not syncing")
	s := e.Run(Check{})
	if got := statusOf(t, s, "Kernel Panic"); got != check.Unhealthy {
		t.Fatalf("status %v", got)
	}
}

func TestWithoutJournalOnlyExistingSourcesAreScanned(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/var/log/messages", "")
	s := e.Run(Check{})
	labels := checktest.Labels(s)
	if slices.Contains(labels, "SSH Brute Force") || slices.Contains(labels, "sudo Escalation") {
		t.Fatalf("auth patterns without an auth log: %q", labels)
	}
	if !slices.Contains(labels, "OOM Killer") || !e.Fake.Ran("'/var/log/messages'") {
		t.Fatalf("system patterns not scanned: %q / %q", labels, e.Fake.Calls())
	}
}

func TestNoLogSourcesAtAll(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	if labels := checktest.Labels(s); !slices.Equal(labels, []string{"Log Sources"}) {
		t.Fatalf("labels: %q", labels)
	}
	if len(e.Fake.Calls()) != 0 {
		t.Fatalf("ran %q", e.Fake.Calls())
	}
}

func TestExtraPatternsAreCheckedAfterTheBuiltIns(t *testing.T) {
	cfg := &Config{Toggle: check.On, SSHBruteForceMin: 50, ExtraPatterns: []Pattern{
		{Label: "NFS Timeouts", Pattern: "nfs: server .* not responding", Severity: check.Caution, MinCount: 3},
	}}
	s := logsEnv(t, cfg, map[string]int{"not responding": 3}).Run(Check{})
	labels := checktest.Labels(s)
	if labels[9] != "NFS Timeouts" {
		t.Fatalf("labels: %q", labels)
	}
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"NFS Timeouts: 3 occurrence(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestValidate(t *testing.T) {
	ok := Check{}.Defaults().(*Config)
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]*Config{
		"ssh min above the scan cap": {SSHBruteForceMin: 500},
		"negative ssh min":           {SSHBruteForceMin: -1},
		"empty label":                {ExtraPatterns: []Pattern{{Pattern: "x", Severity: check.Info}}},
		"label clash":                {ExtraPatterns: []Pattern{{Label: "Kernel Panic", Pattern: "x", Severity: check.Info}}},
		"empty pattern":              {ExtraPatterns: []Pattern{{Label: "X", Severity: check.Info}}},
		"unknown source":             {ExtraPatterns: []Pattern{{Label: "X", Pattern: "x", Source: "kern", Severity: check.Info}}},
		"no severity":                {ExtraPatterns: []Pattern{{Label: "X", Pattern: "x"}}},
		"min count above cap":        {ExtraPatterns: []Pattern{{Label: "X", Pattern: "x", Severity: check.Caution, MinCount: 1000}}},
	}
	for name, c := range cases {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExtraPatternsDecodeFromYAML(t *testing.T) {
	cfg := Check{}.Defaults().(*Config)
	dec := yaml.NewDecoder(strings.NewReader(`
enabled: true
ssh_brute_force_min: 100
extra_patterns:
  - label: NFS Timeouts
    pattern: "nfs: server .* not responding"
    source: system
    severity: caution
    min_count: 3
`))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		t.Fatal(err)
	}
	want := Pattern{"NFS Timeouts", "nfs: server .* not responding", "system", check.Caution, 3}
	if cfg.SSHBruteForceMin != 100 || len(cfg.ExtraPatterns) != 1 || cfg.ExtraPatterns[0] != want {
		t.Fatalf("decoded %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
