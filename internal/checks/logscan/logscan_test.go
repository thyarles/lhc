package logscan

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc-go/internal/checktest"
)

var ctx = context.Background()

func TestTodayREMatchesSyslogPadding(t *testing.T) {
	cases := []struct {
		now  time.Time
		line string
		want bool
	}{
		// syslog space-pads single-digit days.
		{checktest.Now, "Jan  5 07:00:01 host sshd[1]: Failed password", true},
		{checktest.Now, "Jan 5 07:00:01 host sshd[1]: Failed password", true},
		// The 15th is not the 5th, and neither is the 5th of another month.
		{checktest.Now, "Jan 15 07:00:01 host sshd[1]: Failed password", false},
		{checktest.Now, "Feb  5 07:00:01 host sshd[1]: Failed password", false},
		{checktest.Now, "Jan 05 07:00:01 host kernel: zero-padded is not syslog", false},
		{time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), "Jan 15 00:00:01 host x", true},
		{time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), "Jan  5 00:00:01 host x", false},
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "Jan 15 00:00:01 host x", false},
	}
	for _, c := range cases {
		re := regexp.MustCompile(TodayRE(c.now))
		if got := re.MatchString(c.line); got != c.want {
			t.Errorf("TodayRE(%s) = %q on %q: %v, want %v", c.now.Format("Jan 2"), re, c.line, got, c.want)
		}
	}
}

func TestTodayREUsesTheClocksLocation(t *testing.T) {
	// 23:30 UTC on the 4th is already the 5th in UTC+2.
	tz := time.FixedZone("UTC+2", 2*3600)
	now := time.Date(2026, 1, 4, 23, 30, 0, 0, time.UTC).In(tz)
	if got := TodayRE(now); got != "Jan[ ]+5[ ]" {
		t.Fatalf("TodayRE = %q", got)
	}
}

func TestParseCount(t *testing.T) {
	cases := map[string]int{
		"12":        12,
		"0\n0":      0, // grep -c printed 0 and failed, so `|| echo 0` added another
		"  7  \n":   7,
		"":          0,
		"garbage":   0,
		"-3":        0,
		"42\nnoise": 42,
	}
	for in, want := range cases {
		if got := ParseCount(in); got != want {
			t.Errorf("ParseCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"oom.kill|Out of memory": `'oom.kill|Out of memory'`,
		"it's":                   `'it'\''s'`,
		"":                       `''`,
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAuthLogPrefersDebianThenRHEL(t *testing.T) {
	f := checktest.NewRunner()
	if got := AuthLog(f); got != "" {
		t.Fatalf("no log: got %q", got)
	}
	f.File("/var/log/secure", "")
	if got := AuthLog(f); got != "/var/log/secure" {
		t.Fatalf("RHEL: got %q", got)
	}
	f.File("/var/log/auth.log", "")
	if got := AuthLog(f); got != "/var/log/auth.log" {
		t.Fatalf("Debian: got %q", got)
	}
}

func TestSystemLogsListsOnlyWhatExists(t *testing.T) {
	f := checktest.NewRunner()
	f.File("/var/log/messages", "").File("/var/log/dmesg", "")
	if got := SystemLogs(f); !slices.Equal(got, []string{"/var/log/messages", "/var/log/dmesg"}) {
		t.Fatalf("SystemLogs = %q", got)
	}
}

func TestCountUsesTheJournalForToday(t *testing.T) {
	f := checktest.NewRunner()
	f.Tool("journalctl")
	f.Expect("journalctl", "17")
	if got := Count(ctx, f, checktest.Now, "", "Failed password"); got != 17 {
		t.Fatalf("Count = %d", got)
	}
	call := f.Calls()[0]
	for _, want := range []string{"--since=2026-01-05", "grep -cE 'Failed password'", "|| echo 0"} {
		if !strings.Contains(call, want) {
			t.Errorf("%q missing from %q", want, call)
		}
	}
}

func TestCountFallsBackToTodaysLinesOfTheFile(t *testing.T) {
	f := checktest.NewRunner()
	f.File("/var/log/secure", "")
	f.Expect("/var/log/secure", "3\n")
	if got := Count(ctx, f, checktest.Now, "/var/log/secure", `Accepted (password|publickey)`); got != 3 {
		t.Fatalf("Count = %d", got)
	}
	call := f.Calls()[0]
	if !strings.Contains(call, `grep -E 'Jan[ ]+5[ ]' '/var/log/secure'`) ||
		!strings.Contains(call, `grep -cE 'Accepted (password|publickey)'`) {
		t.Fatalf("command: %q", call)
	}
}

func TestCountWithoutJournalOrFileIsZeroAndRunsNothing(t *testing.T) {
	f := checktest.NewRunner()
	if got := Count(ctx, f, checktest.Now, "/var/log/auth.log", "x"); got != 0 {
		t.Fatalf("Count = %d", got)
	}
	if got := Count(ctx, f, checktest.Now, "", "x"); got != 0 {
		t.Fatalf("Count = %d", got)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("ran %q", calls)
	}
}

func TestTodayFromTheJournalKeepsTheLastSamples(t *testing.T) {
	f := checktest.NewRunner()
	f.Tool("journalctl")
	f.Expect("journalctl", "a segfault\n\nb segfault\nc segfault\nd segfault\n")
	n, samples := Today(ctx, f, checktest.Now, []string{"/var/log/syslog"}, "segfault|SIGSEGV")
	if n != 4 || !slices.Equal(samples, []string{"b segfault", "c segfault", "d segfault"}) {
		t.Fatalf("Today = %d, %q", n, samples)
	}
	call := f.Calls()[0]
	if !strings.Contains(call, "grep -iE 'segfault|SIGSEGV' | tail -200") || strings.Contains(call, "/var/log/syslog") {
		t.Fatalf("command: %q", call)
	}
}

func TestTodayFallsBackToEachSourceThatExists(t *testing.T) {
	f := checktest.NewRunner()
	f.File("/var/log/syslog", "").File("/var/log/kern.log", "")
	f.Expect("/var/log/syslog", "s1\ns2")
	f.Expect("/var/log/kern.log", "k1")
	n, samples := Today(ctx, f, checktest.Now,
		[]string{"/var/log/syslog", "/var/log/messages", "/var/log/kern.log"}, "I/O error")
	if n != 3 || !slices.Equal(samples, []string{"s1", "s2", "k1"}) {
		t.Fatalf("Today = %d, %q", n, samples)
	}
	if f.Ran("/var/log/messages") {
		t.Fatal("grepped a source that does not exist")
	}
	if !f.Ran(`grep -E 'Jan[ ]+5[ ]' '/var/log/syslog' 2>/dev/null | grep -iE 'I/O error' | tail -200`) {
		t.Fatalf("calls: %q", f.Calls())
	}
}

func TestTodayWithNothingToReadIsZero(t *testing.T) {
	f := checktest.NewRunner()
	n, samples := Today(ctx, f, checktest.Now, nil, "x")
	if n != 0 || len(samples) != 0 {
		t.Fatalf("Today = %d, %q", n, samples)
	}
}
