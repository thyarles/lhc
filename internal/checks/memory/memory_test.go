package memory

import (
	"slices"
	"strconv"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

const gib = 1 << 30

func free(memUsed, swapTotal, swapUsed int64) string {
	return "               total        used        free      shared  buff/cache   available\n" +
		"Mem:     " + itoa(16*gib) + " " + itoa(memUsed) + " " + itoa(16*gib-memUsed) + " 0 0 0\n" +
		"Swap:    " + itoa(swapTotal) + " " + itoa(swapUsed) + " " + itoa(swapTotal-swapUsed) + "\n"
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRowsAndValueFormat(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", free(4*gib, 2*gib, 0))
	s := e.Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"Mem", "Swap"}) {
		t.Fatalf("labels: %v", got)
	}
	mem := checktest.Rows(s)["Mem"]
	if mem.Value != "25.0% used  (4 GB of 16 GB, 12 GB free)" || mem.Status != check.OK {
		t.Fatalf("Mem row: %+v", mem)
	}
	if mem.Meter == nil || *mem.Meter != 25 {
		t.Fatalf("Mem meter: %v", mem.Meter)
	}
	checktest.NoAlerts(t, s)
}

func TestRAMThresholds(t *testing.T) {
	cases := []struct {
		used  int64
		want  check.Status
		alert bool
	}{
		{12 * gib, check.OK, false},                // 75%
		{13 * gib, check.Caution, false},           // 81.25%: in the report, nobody paged
		{int64(15.5 * gib), check.Unhealthy, true}, // 96.9%
	}
	for _, c := range cases {
		e := checktest.NewEnv(t, Check{}, nil)
		e.Fake.Expect("free -b", free(c.used, 0, 0))
		s := e.Run(Check{})
		if got := checktest.Rows(s)["Mem"].Status; got != c.want {
			t.Errorf("used %d: status %v, want %v", c.used, got, c.want)
		}
		if got := checktest.HasAlertContaining(s, "RAM at"); got != c.alert {
			t.Errorf("used %d: alert %v, want %v (%q)", c.used, got, c.alert, s.AlertMsgs())
		}
	}
}

func TestUnhealthyRAMAlertText(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", free(int64(15.5*gib), 0, 0))
	if got := e.Run(Check{}).AlertMsgs(); !slices.Equal(got, []string{"RAM at 97%"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestFullSwapIsFlaggedButNeverAlerts(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", free(4*gib, 2*gib, 2*gib))
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Swap"].Status; got != check.Unhealthy {
		t.Fatalf("Swap status %v", got)
	}
	checktest.NoAlerts(t, s)
}

func TestNoSwapIsZeroPercent(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", free(4*gib, 0, 0))
	row := checktest.Rows(e.Run(Check{}))["Swap"]
	if row.Value != "0.0% used  (0 B of 0 B, 0 B free)" || row.Status != check.OK {
		t.Fatalf("Swap row: %+v", row)
	}
}

func TestOldFreeBuffersLineIsIgnored(t *testing.T) {
	// procps 3.2 (RHEL 6) prints a "-/+ buffers/cache:" line between them.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", "             total       used       free\n"+
		"Mem:   1024 512 512\n-/+ buffers/cache:  256 768\nSwap:  0 0 0")
	if got := checktest.Labels(e.Run(Check{})); !slices.Equal(got, []string{"Mem", "Swap"}) {
		t.Fatalf("labels: %v", got)
	}
}

func TestDelta(t *testing.T) {
	cases := []struct {
		name string
		seed map[string]float64
		want string
	}{
		{"first ever run shows nothing", nil, ""},
		{"unchanged shows nothing", map[string]float64{"Mem": 25.0}, ""},
		{"under half a point is noise", map[string]float64{"Mem": 24.6}, ""},
		{"a real move is signed", map[string]float64{"Mem": 22.0}, "+3.0 pts since last run"},
		{"a drop is signed too", map[string]float64{"Mem": 30.0}, "-5.0 pts since last run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := checktest.NewEnv(t, Check{}, nil)
			if c.seed != nil {
				e.Seed(t, stateKey, c.seed)
			}
			e.Fake.Expect("free -b", free(4*gib, 0, 0))
			if got := checktest.Rows(e.Run(Check{}))["Mem"].Delta; got != c.want {
				t.Fatalf("delta %q, want %q", got, c.want)
			}
		})
	}
}

func TestSavesRoundedPercentages(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", "Mem: 3 1 2\nSwap: 0 0 0")
	e.Run(Check{})
	var saved map[string]float64
	if !e.Loaded(t, stateKey, &saved) || saved["Mem"] != 33.3 || saved["Swap"] != 0 {
		t.Fatalf("saved: %v", saved)
	}
}

func TestFmtBytes(t *testing.T) {
	cases := map[int64]string{
		0:                 "0 B",
		1023:              "1023 B",
		1024:              "1 KB",
		1536:              "1 KB", // integer division, never "1.5 KB"
		5 * gib:           "5 GB",
		1 << 50:           "1 PB",
		3 << 60:           "3072 PB",
		1<<40 + (1 << 39): "1 TB",
	}
	for n, want := range cases {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", n, got, want)
		}
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

func TestTheRAMFactIsTheMemRowNotSwap(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("free -b", free(16*gib*85/100, 4*gib, 4*gib))
	got := checktest.Facts(e.Run(Check{}))["ram"]
	if got != (check.Fact{Key: "ram", Value: "85%", Status: check.Caution}) {
		t.Fatalf("ram fact = %+v", got)
	}
}
