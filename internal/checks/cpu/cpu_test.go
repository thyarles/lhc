package cpu

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

func mpstatLine(id string, idle float64) string {
	return fmt.Sprintf("Average:  %s  1.0  0.0  1.0  0.0  0.0  0.0  0.0  0.0  0.0  %.1f", id, idle)
}

func newEnv(t *testing.T) *checktest.Env {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/proc/loadavg", "1.00 1.00 1.00 1/200 1234\n")
	return e
}

func TestPerCoreCPUIsSummarisedNotListed(t *testing.T) {
	// 96 rows saying "0.0% used" were 29% of the whole report.
	e := newEnv(t)
	e.Fake.Tool("mpstat")
	lines := []string{mpstatLine("all", 99)}
	for i := range 96 {
		lines = append(lines, mpstatLine(fmt.Sprint(i), 99))
	}
	e.Fake.Expect("mpstat", strings.Join(lines, "\n"))
	e.Fake.Expect("nproc", "96")
	s := e.Run(Check{})
	labels := checktest.Labels(s)
	if !slices.Contains(labels, "CPU (all cores)") || !slices.Contains(labels, "Cores") {
		t.Fatalf("missing summary rows: %v", labels)
	}
	for _, l := range labels {
		if strings.HasPrefix(l, "CPU ") && l != "CPU (all cores)" {
			t.Fatalf("idle core got its own row: %q", l)
		}
	}
	if len(s.Rows) >= 10 {
		t.Fatalf("%d rows for an idle 96-core box", len(s.Rows))
	}
	checktest.NoAlerts(t, s)
}

func TestBusyCoresAreStillNamed(t *testing.T) {
	e := newEnv(t)
	e.Fake.Tool("mpstat")
	lines := []string{mpstatLine("all", 50)}
	for i := range 8 {
		lines = append(lines, mpstatLine(fmt.Sprint(i), 99))
	}
	lines = append(lines, mpstatLine("9", 2)) // 98% busy
	e.Fake.Expect("mpstat", strings.Join(lines, "\n"))
	e.Fake.Expect("nproc", "10")
	s := e.Run(Check{})
	row, ok := checktest.Rows(s)["CPU 9"]
	if !ok || row.Value != "98.0% used" || row.Status != check.Unhealthy {
		t.Fatalf("CPU 9 row: %+v", row)
	}
	if !checktest.HasAlertContaining(s, "CPU 9 at 98%") {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
}

func TestMpstatSnapshotThenAverageKeepsTheAverage(t *testing.T) {
	out := "12:00:01 PM  all  1 0 1 0 0 0 0 0 0 10.0\nAverage:     all  1 0 1 0 0 0 0 0 0 90.0"
	rows := parseMpstat(out, Check{}.Defaults().(*Config))
	if got := rows["all"].used; got != 10 {
		t.Fatalf("used = %v, want the Average line (10)", got)
	}
}

func TestMpstatCommaDecimalLocale(t *testing.T) {
	rows := parseMpstat("Average:  all  1,0 0 0 0 0 0 0 0 0 75,5", Check{}.Defaults().(*Config))
	if got := rows["all"].used; got != 24.5 {
		t.Fatalf("used = %v", got)
	}
}

func TestLoadThresholdsScaleWithCores(t *testing.T) {
	cases := []struct {
		load string
		want check.Status
	}{
		{"3.99 1 1", check.OK},
		{"4.00 1 1", check.Caution},
		{"8.00 1 1", check.Unhealthy},
	}
	for _, c := range cases {
		e := checktest.NewEnv(t, Check{}, nil)
		e.Fake.File("/proc/loadavg", c.load)
		e.Fake.Expect("nproc", "4")
		s := e.Run(Check{})
		if got := checktest.Rows(s)["Load Average (1/5/15 min)"].Status; got != c.want {
			t.Errorf("load %s on 4 CPUs: %v, want %v", c.load, got, c.want)
		}
		// Only UNHEALTHY load interrupts anyone; caution is in the report.
		if c.want != check.Unhealthy {
			checktest.NoAlerts(t, s)
		}
	}
}

func TestWithoutMpstatFallsBackAndAsksForSysstat(t *testing.T) {
	e := newEnv(t)
	e.Fake.File("/proc/stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 1 1 1\nbtime 1700000000\n")
	s := e.Run(Check{})
	row, ok := checktest.Rows(s)["CPU (aggregate, approx.)"]
	if !ok || row.Value != "20.0% used" {
		t.Fatalf("aggregate row: %+v", row)
	}
	if len(s.MissingTools) != 1 || s.MissingTools[0].Package("apt-get") != "sysstat" {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
}

func TestValidate(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Caution = 99
	if c.Validate() == nil {
		t.Fatal("caution above unhealthy accepted")
	}
}

func TestTheLoadFact(t *testing.T) {
	for _, c := range []struct {
		nproc, load, want string
		st                check.Status
	}{
		{"8", "0.42 0.30 0.20 1/200 1234", "0.42 / 8 cores", check.OK},
		{"1", "2.50 1.00 1.00 1/200 1234", "2.50 / 1 core", check.Unhealthy},
	} {
		e := checktest.NewEnv(t, Check{}, nil)
		e.Fake.File("/proc/loadavg", c.load)
		e.Fake.Expect("nproc", c.nproc)
		got := checktest.Facts(e.Run(Check{}))["load"]
		if got.Value != c.want || got.Status != c.st {
			t.Errorf("load fact = %+v, want %q %v", got, c.want, c.st)
		}
	}
}
