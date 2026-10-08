package system

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

func env(t *testing.T, btime string) *checktest.Env {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/proc/stat", "cpu 1 2 3 4\nbtime "+btime+"\n")
	return e
}

func TestRebootSinceLastRunIsFlagged(t *testing.T) {
	e := env(t, "1700500000")
	e.Seed(t, "boot_time", 1_700_000_000.0)
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"Host rebooted since the previous run"}) {
		t.Fatalf("alerts: %q", got)
	}
	if checktest.Rows(s)["Last Boot"].Status != check.Caution {
		t.Fatal("Last Boot row not flagged")
	}
}

func TestNoRebootMeansNoAlert(t *testing.T) {
	e := env(t, "1700000000")
	e.Seed(t, "boot_time", 1_700_000_000.0)
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestFirstRunIsNotAReboot(t *testing.T) {
	e := env(t, "1700000000")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	var saved float64
	if !e.Loaded(t, "boot_time", &saved) || saved != 1_700_000_000 {
		t.Fatalf("boot time not saved: %v", saved)
	}
}

func TestSmallBootTimeJitterIsNotAReboot(t *testing.T) {
	// btime is derived from uptime and can move by a second between reads.
	e := env(t, "1700000001")
	e.Seed(t, "boot_time", 1_700_000_000.0)
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestResolvedNameShownWhenItDisagrees(t *testing.T) {
	e := env(t, "1700000000")
	e.Host = check.Host{Label: "web-node03", Kernel: "web-node03", Resolved: "cluster-vip.example.com"}
	rows := checktest.Rows(e.Run(Check{}))
	if rows["Hostname"].Value != "web-node03" {
		t.Fatalf("Hostname = %q", rows["Hostname"].Value)
	}
	if !strings.Contains(rows["Resolved name"].Value, "cluster-vip.example.com") {
		t.Fatalf("Resolved name row missing: %+v", rows)
	}
}

func TestResolvedNameQuietWhenNamesAgree(t *testing.T) {
	e := env(t, "1700000000")
	e.Host = check.Host{Label: "web01.example.com", Kernel: "web01", Resolved: "web01.example.com"}
	if slices.Contains(checktest.Labels(e.Run(Check{})), "Resolved name") {
		t.Fatal("Resolved name shown although the names agree")
	}
}

func TestOSAndCPUModel(t *testing.T) {
	e := env(t, "1700000000")
	e.Host.OS = map[string]string{"PRETTY_NAME": "Rocky Linux 9.4 (Blue Onyx)"}
	e.Fake.Expect("lscpu", "Architecture: x86_64\nModel name:   Intel(R) Xeon(R)   Gold 6248\n")
	e.Fake.Expect("uname -r", "5.14.0-427.el9.x86_64")
	rows := checktest.Rows(e.Run(Check{}))
	if rows["OS"].Value != "Rocky Linux 9.4 (Blue Onyx)" {
		t.Errorf("OS = %q", rows["OS"].Value)
	}
	if rows["CPU Model"].Value != "Intel(R) Xeon(R) Gold 6248" {
		t.Errorf("CPU Model = %q", rows["CPU Model"].Value)
	}
	if rows["Kernel"].Value != "5.14.0-427.el9.x86_64" {
		t.Errorf("Kernel = %q", rows["Kernel"].Value)
	}
}

func TestUptime(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:       "up 0 minutes",
		61 * time.Minute:       "up 1 hour, 1 minute",
		(9*24 + 2) * time.Hour: "up 1 week, 2 days, 2 hours",
	}
	for d, want := range cases {
		if got := Uptime(d); got != want {
			t.Errorf("Uptime(%v) = %q, want %q", d, got, want)
		}
	}
}
