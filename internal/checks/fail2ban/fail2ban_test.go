package fail2ban

import (
	"slices"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

const status = "Status\n|- Number of jail:\t1\n`- Jail list:\tsshd"

func newEnv(t *testing.T, cfg *Config) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	e.Fake.Tool("fail2ban-client")
	return e
}

func TestBansAreTheSystemWorking(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("fail2ban-client status sshd", "Currently banned:\t14\nTotal banned:\t320")
	e.Fake.Expect("fail2ban-client status", status)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	row := checktest.Rows(s)["Jail: sshd"]
	if row.Status != check.Info || row.Value != "14 currently banned / 320 total" {
		t.Fatalf("jail row: %+v", row)
	}
}

func TestSpikeThresholdCanBeEnabled(t *testing.T) {
	e := newEnv(t, &Config{Toggle: check.On, BannedIPsCaution: 10})
	e.Fake.Expect("fail2ban-client status sshd", "Currently banned:\t14\nTotal banned:\t320")
	e.Fake.Expect("fail2ban-client status", status)
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"fail2ban: unusual ban volume — 14 IP(s) currently banned"}) {
		t.Fatalf("alerts: %q", got)
	}
	if row := checktest.Rows(s)["Ban Volume"]; row.Value != "14 IPs banned (spike threshold 10)" || row.Status != check.Caution {
		t.Fatalf("Ban Volume row: %+v", row)
	}
}

func TestSpikeIsSummedOverJails(t *testing.T) {
	e := newEnv(t, &Config{Toggle: check.On, BannedIPsCaution: 10})
	e.Fake.Expect("fail2ban-client status sshd", "Currently banned:\t6\nTotal banned:\t60")
	e.Fake.Expect("fail2ban-client status nginx-http-auth", "Currently banned:\t5\nTotal banned:\t9")
	e.Fake.Expect("fail2ban-client status", "Status\n|- Number of jail:\t2\n`- Jail list:\tsshd, nginx-http-auth")
	s := e.Run(Check{})
	if len(s.Alerts) != 1 {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
	if labels := checktest.Labels(s); !slices.Equal(labels, []string{"Jail: sshd", "Jail: nginx-http-auth", "Ban Volume"}) {
		t.Fatalf("labels: %q", labels)
	}
}

func TestQuietJailIsOK(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("fail2ban-client status sshd", "Currently banned:\t0\nTotal banned:\t3")
	e.Fake.Expect("fail2ban-client status", status)
	s := e.Run(Check{})
	if s.Status != check.OK {
		t.Fatalf("status %v", s.Status)
	}
}

func TestNotInstalledIsNotApplicable(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	if s.Applicable {
		t.Fatal("applicable without fail2ban-client")
	}
	if len(s.MissingTools) != 1 || s.MissingTools[0].Package("apt-get") != "fail2ban" || s.MissingTools[0].Optional {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
	checktest.NoAlerts(t, s)
}

func TestServiceNotRunningIsCaution(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("fail2ban-client status", "").Code(255)
	s := e.Run(Check{})
	row := checktest.Rows(s)["fail2ban"]
	if row.Value != "Service not running or no output" || row.Status != check.Caution {
		t.Fatalf("row: %+v", row)
	}
	checktest.NoAlerts(t, s)
}

func TestEmptyJailListIsInformational(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("fail2ban-client status", "Status\n|- Number of jail:\t0\n`- Jail list:")
	row := checktest.Rows(e.Run(Check{}))["fail2ban"]
	if row.Status != check.Info {
		t.Fatalf("row: %+v", row)
	}
}

func TestValidate(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.BannedIPsCaution = -1
	if c.Validate() == nil {
		t.Fatal("negative threshold accepted")
	}
}
