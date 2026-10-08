package schedule

import (
	"bytes"
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/checktest"
	"github.com/thyarles/lhc/internal/config"
)

// The bug this answers: every host installed the same time, so they all began
// a full scan at once — on servers also running backups — and reported the
// CPU spike they caused, every day, to people for whom that CPU is normal.

func TestWindowsPeopleActuallyWrite(t *testing.T) {
	cases := map[string]time.Duration{
		"4h": 4 * time.Hour, "90m": 90 * time.Minute, "2h30m": 150 * time.Minute, "2h 30m": 150 * time.Minute,
		"45s": 45 * time.Second, "0": 0, "  3h  ": 3 * time.Hour, "4H": 4 * time.Hour, "1d": 24 * time.Hour,
	}
	for in, want := range cases {
		if got, err := ParseWindow(in); err != nil || got != want {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestABareNumberMeansHours(t *testing.T) {
	// "4" meaning four seconds would be a trap.
	if got, _ := ParseWindow("4"); got != 4*time.Hour {
		t.Fatal(got)
	}
	if got, _ := ParseWindow("0.5"); got != 30*time.Minute {
		t.Fatal(got)
	}
}

func TestUnreadableWindowsAreRejected(t *testing.T) {
	for _, junk := range []string{"", "   ", "soon", "4x", "4h junk", "h", "-3"} {
		if _, err := ParseWindow(junk); err == nil {
			t.Errorf("%q accepted", junk)
		}
	}
}

func TestATypoFallsBackToTheDefaultNotToZero(t *testing.T) {
	var log bytes.Buffer
	if got := Window("4 hours!", &log); got != DefaultWindow {
		t.Fatal(got)
	}
	if !strings.Contains(log.String(), "random_window") {
		t.Fatal("no warning logged")
	}
}

func TestEveryDrawIsInsideTheWindowOnAWholeMinute(t *testing.T) {
	for seed := range uint64(200) {
		d := Draw(2*time.Hour, rand.New(rand.NewPCG(seed, 1)))
		if d < 0 || d > 2*time.Hour || d%time.Minute != 0 {
			t.Fatalf("seed %d: %v", seed, d)
		}
	}
}

func TestTheWholeWindowIsReachable(t *testing.T) {
	// An off-by-one that capped the draw short would leave the tail empty.
	lo, hi := time.Hour, time.Duration(0)
	for seed := range uint64(1000) {
		d := Draw(time.Hour, rand.New(rand.NewPCG(seed, 2)))
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo != 0 || hi != time.Hour {
		t.Fatalf("range [%v, %v]", lo, hi)
	}
}

func TestAZeroWindowIsTheSameAsOff(t *testing.T) {
	if Draw(0, nil) != 0 {
		t.Fatal("zero window drew a delay")
	}
}

func TestTwoHostsDrawingIndependentlyDoNotAgree(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 20 {
		seen[Draw(4*time.Hour, nil)] = true
	}
	if len(seen) < 2 {
		t.Fatal("every draw identical")
	}
}

func TestDurationsReadLikeAClock(t *testing.T) {
	cases := map[time.Duration]string{0: "0m", time.Minute: "1m", 9420 * time.Second: "2h37m", time.Hour: "1h00m", 4 * time.Hour: "4h00m"}
	for d, want := range cases {
		if got := FmtDuration(d); got != want {
			t.Errorf("FmtDuration(%v) = %q", d, got)
		}
	}
}

func TestTheWaitIsAnnouncedBeforeItStarts(t *testing.T) {
	// Hours of silence between the timer and the first check look exactly
	// like a hang. The log must say it is waiting, and until when, first.
	fixed := time.Date(2026, 9, 21, 7, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	var log syncBuf
	done := make(chan time.Duration)
	go func() {
		done <- Delay(ctx, config.Schedule{Random: true, RandomWindow: "2h"}, &log, func() time.Time { return fixed })
	}()
	deadline := time.Now().Add(2 * time.Second)
	for log.String() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	out := log.String()
	if !strings.Contains(out, "starting now") && !strings.Contains(out, "checks start at") {
		t.Fatalf("nothing announced: %q", out)
	}
}

func TestADisabledDelaySleepsForNothing(t *testing.T) {
	var log bytes.Buffer
	if Delay(context.Background(), config.Schedule{Random: false, RandomWindow: "8h"}, &log, time.Now) != 0 || log.Len() != 0 {
		t.Fatal("disabled delay waited or logged")
	}
}

func sched(at, every string) config.Schedule {
	s := config.Schedule{Time: at, Random: true, RandomWindow: "8h"}
	if every != "" {
		d, _ := config.ParseDuration(every)
		s.Every = config.Duration(d)
	}
	return s
}

func TestCalendarAndCronLines(t *testing.T) {
	j := Job{Binary: "/usr/local/bin/lhc", Config: "/etc/lhc/config.yaml", Log: "/var/log/lhc.log"}
	cases := []struct{ at, every, cal, cron string }{
		{"00:07", "", "*-*-* 00:07:00", "7 0 * * *"},
		{"06:30", "", "*-*-* 06:30:00", "30 6 * * *"},
		{"00:07", "6h", "*-*-* 00,06,12,18:07:00", "7 0,6,12,18 * * *"},
		{"13:15", "8h", "*-*-* 05,13,21:15:00", "15 5,13,21 * * *"},
	}
	for _, c := range cases {
		s := sched(c.at, c.every)
		cal, err := OnCalendar(s)
		if err != nil || cal != c.cal {
			t.Errorf("OnCalendar(%s/%s) = %q, %v", c.at, c.every, cal, err)
		}
		line, err := CronLine(s, j)
		if err != nil || !strings.HasPrefix(line, c.cron+" ") {
			t.Errorf("CronLine(%s/%s) = %q, %v", c.at, c.every, line, err)
		}
	}
}

func TestTheCronEntryMarksTheRunAsScheduled(t *testing.T) {
	// The random delay applies to the scheduled run and NOT to `lhc run`
	// typed at a prompt; only the flag in the entry tells them apart.
	line, _ := CronLine(sched("07:00", ""), Job{Binary: "/b/lhc", Config: "/c.yaml", Log: "/l"})
	if !strings.Contains(line, " run --scheduled ") || !strings.HasSuffix(line, CronTag) || strings.Contains(line, "random") {
		t.Fatal(line)
	}
}

func TestTimerUsesSystemdJitterAndTheServiceDoesNotAddMore(t *testing.T) {
	timer, err := TimerUnit(sched("00:07", ""))
	if err != nil || !strings.Contains(timer, "RandomizedDelaySec=28800") || !strings.Contains(timer, "Persistent=true") {
		t.Fatal(timer, err)
	}
	if svc := ServiceUnit(Job{Binary: "/b/lhc", Config: "/c.yaml"}); !strings.Contains(svc, "run --scheduled --no-random") {
		t.Fatal(svc)
	}
	s := sched("00:07", "")
	s.Random = false
	if timer, _ := TimerUnit(s); strings.Contains(timer, "RandomizedDelaySec") {
		t.Fatal("jitter with random: false")
	}
}

func TestTheWindowNeverOutlastsTheGapBetweenRuns(t *testing.T) {
	timer, _ := TimerUnit(sched("00:07", "6h"))
	if !strings.Contains(timer, "RandomizedDelaySec=21540") {
		t.Fatal(timer)
	}
}

func TestMergeCrontabReplacesOnlyOurLine(t *testing.T) {
	cur := "MAILTO=root\n0 1 * * * /backup.sh\n7 0 * * * /old/lhc run --scheduled  # lhc-managed\n"
	got := MergeCrontab(cur, "8 0 * * * /new/lhc  # lhc-managed")
	want := "MAILTO=root\n0 1 * * * /backup.sh\n8 0 * * * /new/lhc  # lhc-managed\n"
	if got != want {
		t.Fatalf("%q", got)
	}
	if MergeCrontab(got, "") != "MAILTO=root\n0 1 * * * /backup.sh\n" {
		t.Fatal("removal left our line")
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name    string
		root    bool
		systemd string
		cron    bool
		want    Backend
	}{
		{"modern systemd as root", true, "systemd 252 (252.22-1~deb12u1)", true, Systemd},
		{"RHEL 7 systemd 219 gets cron", true, "systemd 219\n+PAM +AUDIT", true, Cron},
		{"non-root never gets system units", false, "systemd 252", true, Cron},
		{"no systemd and no cron", true, "", false, None},
	}
	for _, c := range cases {
		f := checktest.NewRunner()
		if c.systemd != "" {
			f.Dir("/run/systemd/system")
			f.Expect("systemctl --version", c.systemd)
		}
		if c.cron {
			f.Tool("crontab")
		}
		if got := Detect(context.Background(), f, c.root); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)
	if n, _ := Next(sched("00:07", ""), now); !n.Equal(time.Date(2026, 1, 6, 0, 7, 0, 0, time.UTC)) {
		t.Fatal(n)
	}
	if n, _ := Next(sched("00:07", "6h"), now); !n.Equal(time.Date(2026, 1, 5, 12, 7, 0, 0, time.UTC)) {
		t.Fatal(n)
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(sched("00:07", "")); got != "fires daily at 00:07, then waits a random 0–8h00m before running" {
		t.Fatal(got)
	}
}
