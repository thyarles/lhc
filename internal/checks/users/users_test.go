package users

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

func TestHistoricRootLoginsAreInformational(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("last -n 5 root", strings.Join([]string{
		"root     pts/3        Thu Jul 30 16:46 - crash  (22:52)",
		"root     pts/3        Wed Jul 29 13:22 - crash (1+00:23)",
	}, "\n"))
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if f := checktest.Flagged(s); len(f) > 0 {
		t.Fatalf("flagged rows: %+v", f)
	}
}

func TestRootLoginTodayAlerts(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("last -n 5 root", strings.Join([]string{
		checktest.Now.Format("root     pts/8        Mon Jan _2 12:34   still logged in"),
		"root     pts/3        Thu Jul 30 16:46 - crash  (22:52)",
	}, "\n"))
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"1 root login(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestRootLoginTodayMatchesSingleDigitDays(t *testing.T) {
	// `last` space-pads single-digit days ("Aug  5"); a zero-padded date
	// would never match.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Now = func() time.Time { return time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC) }
	e.Fake.Expect("last -n 5 root", "root     pts/8        Wed Aug  5 12:34   still logged in")
	if got := e.Run(Check{}).AlertMsgs(); !slices.Equal(got, []string{"1 root login(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestRootLoginOnThe15thIsNotTodayOnThe1st(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Now = func() time.Time { return time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) }
	e.Fake.Expect("last -n 5 root", "root     pts/8        Thu Jan 15 12:34 - 13:00  (00:26)")
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestRootLoginRowsAndWtmpFooter(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("last -n 5 root", "root     pts/8        Mon Jan  5 06:00   still logged in\n\nwtmp begins Thu Jan  1 00:00:00 2026")
	s := e.Run(Check{})
	var root []check.Row
	for _, r := range s.Rows {
		if r.Label == "root" {
			root = append(root, r)
		}
	}
	if len(root) != 1 || root[0].Status != check.Caution {
		t.Fatalf("root rows: %+v", root)
	}
	if !slices.Contains(checktest.Labels(s), "Recent Root Logins") {
		t.Fatalf("labels: %q", checktest.Labels(s))
	}
}

func TestNoRootLoginsMeansNoRootSeparator(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("last -n 5 root", "\nwtmp begins Thu Jan  1 00:00:00 2026")
	s := e.Run(Check{})
	if slices.Contains(checktest.Labels(s), "Recent Root Logins") {
		t.Fatal("root separator with no root logins")
	}
}

func TestLoggedInUsersAndRecentLogins(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("who", "alice    pts/0        2026-01-05 06:50 (10.0.0.5)\nbob      pts/1        2026-01-05 06:55 (10.0.0.6)")
	e.Fake.Expect("last -n 5 root", "")
	e.Fake.Expect("last -n 5", strings.Join([]string{
		"alice    pts/0        10.0.0.5         Mon Jan  5 06:50   still logged in",
		"reboot   system boot  5.14.0           Mon Jan  5 06:00   still running",
		"bob      pts/1        10.0.0.6         Mon Jan  5 06:55   still logged in",
		"",
		"wtmp begins Thu Jan  1 00:00:00 2026",
	}, "\n"))
	s := e.Run(Check{})
	var logged, recent int
	for _, r := range s.Rows {
		switch {
		case r.Label == "Logged In":
			logged++
			if r.Status != check.Info {
				t.Errorf("logged-in row %+v", r)
			}
		case r.Label == "" && !r.Separator:
			recent++
			if strings.HasPrefix(r.Value, "reboot") || strings.HasPrefix(r.Value, "wtmp") {
				t.Errorf("noise row %q", r.Value)
			}
		}
	}
	if logged != 2 || recent != 2 {
		t.Fatalf("logged=%d recent=%d rows=%+v", logged, recent, s.Rows)
	}
	checktest.NoAlerts(t, s)
}

func TestNobodyLoggedIn(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	row := checktest.Rows(s)["Logged In"]
	if row.Value != "None" || row.Status != check.OK {
		t.Fatalf("Logged In row: %+v", row)
	}
	if s.Status != check.OK {
		t.Fatalf("status %v", s.Status)
	}
}

func TestLongLinesAreTruncated(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("last -n 5 root", "")
	e.Fake.Expect("last -n 5", strings.Repeat("x", 200))
	for _, r := range e.Run(Check{}).Rows {
		if r.Label == "" && !r.Separator && len(r.Value) != 90 {
			t.Fatalf("row not cut at 90: %d", len(r.Value))
		}
	}
}
