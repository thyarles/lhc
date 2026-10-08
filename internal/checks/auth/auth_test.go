package auth

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

func TestFailedSSHThresholds(t *testing.T) {
	cases := []struct {
		count int
		want  check.Status
		alert string
	}{
		{0, check.OK, ""},
		{1, check.Info, ""},
		{49, check.Info, ""},
		{50, check.Caution, "50 failed SSH attempts today"},
		{500, check.Unhealthy, "500 failed SSH attempts today"},
	}
	for _, c := range cases {
		e := checktest.NewEnv(t, Check{}, nil)
		e.Fake.Tool("journalctl")
		e.Fake.Expect("grep -cE 'Failed password'", fmt.Sprint(c.count))
		s := e.Run(Check{})
		row := checktest.Rows(s)["Failed SSH Attempts (today)"]
		if row.Status != c.want || row.Value != fmt.Sprint(c.count) {
			t.Errorf("%d failures: row %+v, want %v", c.count, row, c.want)
		}
		if row.Detail != "Thresholds: caution ≥ 50, unhealthy ≥ 500" {
			t.Errorf("detail %q", row.Detail)
		}
		got := s.AlertMsgs()
		if c.alert == "" && len(got) > 0 || c.alert != "" && !slices.Equal(got, []string{c.alert}) {
			t.Errorf("%d failures: alerts %q, want %q", c.count, got, c.alert)
		}
	}
}

func TestBackgroundProbingBelowTheConfiguredThresholdIsQuiet(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, &Config{Toggle: check.On, FailedSSHCaution: 1000, FailedSSHUnhealthy: 5000})
	e.Fake.Tool("journalctl")
	e.Fake.Expect("grep -cE 'Failed password'", "800")
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestTopAttackingIPsFromTheAuthLog(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, &Config{Toggle: check.On, FailedSSHCaution: 3, FailedSSHUnhealthy: 500})
	e.Fake.File("/var/log/auth.log", "")
	e.Fake.Expect("grep -cE 'Failed password'", "6")
	var log []string
	for _, ip := range []string{"203.0.113.9", "198.51.100.7", "203.0.113.9", "203.0.113.9", "192.0.2.1", "198.51.100.7"} {
		log = append(log, "Jan  5 06:00:00 host sshd[1]: Failed password for root from "+ip+" port 22 ssh2")
	}
	e.Fake.Expect("grep -F 'Failed password' '/var/log/auth.log'", strings.Join(log, "\n"))
	s := e.Run(Check{})
	labels := checktest.Labels(s)
	i := slices.Index(labels, "Top Attacking IPs")
	if i < 0 || !slices.Equal(labels[i+1:i+4], []string{"203.0.113.9", "198.51.100.7", "192.0.2.1"}) {
		t.Fatalf("labels: %q", labels)
	}
	rows := checktest.Rows(s)
	if r := rows["203.0.113.9"]; r.Value != "3 attempts" || r.Status != check.Caution {
		t.Errorf("top IP row %+v", r)
	}
	if r := rows["192.0.2.1"]; r.Value != "1 attempts" || r.Status != check.Info {
		t.Errorf("single IP row %+v", r)
	}
}

func TestTopIPsAreCappedAtFive(t *testing.T) {
	var b strings.Builder
	for i := range 8 {
		fmt.Fprintf(&b, "Failed password for x from 10.0.0.%d port 1\n", i)
	}
	if got := topIPs(b.String(), 5); len(got) != 5 {
		t.Fatalf("got %d", len(got))
	}
}

func TestNoFailuresMeansNoIPLookup(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/var/log/secure", "")
	e.Run(Check{})
	if e.Fake.Ran("grep -F") {
		t.Fatal("looked up attacking IPs with no failures")
	}
}

func TestSuccessfulLoginsAndSudo(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/var/log/secure", "")
	e.Fake.Expect("grep -cE 'Accepted (password|publickey)'", "4")
	e.Fake.Expect("grep -cE 'sudo.*COMMAND'", "2")
	e.Fake.Expect("grep -E 'sudo.*COMMAND' '/var/log/secure'",
		"Jan  5 06:01:00 host sudo: alice : TTY=pts/0 ; USER=root ; COMMAND=/bin/ls\n"+
			"Jan  5 06:02:00 host sudo: alice : TTY=pts/0 ; USER=root ; COMMAND=/bin/id")
	s := e.Run(Check{})
	rows := checktest.Rows(s)
	if rows["Successful SSH Logins (today)"].Value != "4" || rows["Sudo Commands (today)"].Value != "2" {
		t.Fatalf("rows: %+v", rows)
	}
	if !e.Fake.Ran(`grep -E 'Jan[ ]+5[ ]' | tail -5`) {
		t.Fatalf("sudo listing not scoped to today: %q", e.Fake.Calls())
	}
	labels := checktest.Labels(s)
	if i := slices.Index(labels, "Recent sudo Commands"); i < 0 || len(labels) < i+3 {
		t.Fatalf("labels: %q", labels)
	}
	checktest.NoAlerts(t, s)
	if s.Status != check.Info {
		t.Fatalf("status %v", s.Status)
	}
}

func TestMissingAuthLogOnAJournaldHostIsInformational(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("journalctl")
	row := checktest.Rows(e.Run(Check{}))["Auth Log"]
	if row.Value != "Not found — using journalctl" || row.Status != check.Info {
		t.Fatalf("Auth Log row: %+v", row)
	}
}

func TestMissingAuthLogWithoutJournalIsCaution(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	row := checktest.Rows(s)["Auth Log"]
	if row.Value != "Not found (/var/log/auth.log or /var/log/secure)" || row.Status != check.Caution {
		t.Fatalf("Auth Log row: %+v", row)
	}
	checktest.NoAlerts(t, s)
}

func TestAuthLogPresentHasNoAuthLogRow(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/var/log/auth.log", "")
	if _, ok := checktest.Rows(e.Run(Check{}))["Auth Log"]; ok {
		t.Fatal("Auth Log row although the log exists")
	}
}

func TestValidate(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.FailedSSHCaution = 600
	if c.Validate() == nil {
		t.Fatal("caution above unhealthy accepted")
	}
	c.FailedSSHCaution = -1
	if c.Validate() == nil {
		t.Fatal("negative caution accepted")
	}
}
