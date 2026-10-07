package processes

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

const psHeader = "USER         PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND"

func psLine(pid int, cpu, mem, stat, cmd string) string {
	return fmt.Sprintf("root  %d %s %s 1000 500 ?  %s  Jan01  0:00 %s", pid, cpu, mem, stat, cmd)
}

func withZombies(n int) string {
	lines := []string{psHeader, psLine(1, "0.0", "0.1", "Ss", "/sbin/init")}
	for i := range n {
		lines = append(lines, psLine(1000+i, "0.0", "0.0", "Z", "[defunct-child] <defunct>"))
	}
	return strings.Join(lines, "\n")
}

func TestZombieThresholds(t *testing.T) {
	cases := []struct {
		count int
		want  check.Status
	}{
		{0, check.OK}, {1, check.Info}, {3, check.Info}, {10, check.Caution}, {50, check.Unhealthy},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.count), func(t *testing.T) {
			e := checktest.NewEnv(t, Check{}, nil)
			e.Fake.Expect("ps aux", withZombies(c.count))
			s := e.Run(Check{})
			row := checktest.Rows(s)["Zombie Processes"]
			if row.Status != c.want || row.Value != fmt.Sprint(c.count) {
				t.Fatalf("row %+v, want status %v", row, c.want)
			}
			// A few zombies are routine; only a pile notifies anyone.
			if c.want.Flagged() {
				want := []string{fmt.Sprintf("%d zombie processes", c.count)}
				if !slices.Equal(s.AlertMsgs(), want) || s.Alerts[0].Status != c.want {
					t.Fatalf("alerts %+v", s.Alerts)
				}
			} else {
				checktest.NoAlerts(t, s)
			}
		})
	}
}

func TestZombieThresholdsComeFromTheConfig(t *testing.T) {
	cfg := &Config{Toggle: check.On, ZombieCaution: 2, ZombieUnhealthy: 3}
	e := checktest.NewEnv(t, Check{}, cfg)
	e.Fake.Expect("ps aux", withZombies(2))
	row := checktest.Rows(e.Run(Check{}))["Zombie Processes"]
	if row.Status != check.Caution || row.Detail != "Thresholds: caution ≥ 2, unhealthy ≥ 3" {
		t.Fatalf("row %+v", row)
	}
}

func TestZombiesWithStatModifiersAreCounted(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", strings.Join([]string{psHeader,
		psLine(10, "0.0", "0.0", "Z", "a"),
		psLine(11, "0.0", "0.0", "Z+", "b"),
		psLine(12, "0.0", "0.0", "Zs", "c"),
		psLine(13, "0.0", "0.0", "S", "Zombie-named-but-sleeping"),
	}, "\n"))
	if v := checktest.Rows(e.Run(Check{}))["Zombie Processes"].Value; v != "3" {
		t.Fatalf("zombies = %s", v)
	}
}

func TestTopFiveByMemoryAndByCPU(t *testing.T) {
	lines := []string{psHeader}
	for i := range 8 {
		// memory rises with the pid, CPU falls with it
		lines = append(lines, psLine(100+i, fmt.Sprintf("%d.0", 8-i), fmt.Sprintf("%d.5", i), "S", fmt.Sprintf("proc%d", i)))
	}
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", strings.Join(lines, "\n"))
	s := e.Run(Check{})

	var got []string
	for _, r := range s.Rows {
		if r.Separator {
			got = append(got, "--"+r.Label)
			continue
		}
		got = append(got, r.Label+"="+r.Value)
	}
	want := []string{
		"--Top 5 by Memory",
		"PID 107=7.5% MEM  proc7", "PID 106=6.5% MEM  proc6", "PID 105=5.5% MEM  proc5",
		"PID 104=4.5% MEM  proc4", "PID 103=3.5% MEM  proc3",
		"--Top 5 by CPU",
		"PID 100=8.0% CPU  proc0", "PID 101=7.0% CPU  proc1", "PID 102=6.0% CPU  proc2",
		"PID 103=5.0% CPU  proc3", "PID 104=4.0% CPU  proc4",
		"Zombie Processes=0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if s.Status != check.Info {
		t.Fatalf("status %v", s.Status)
	}
	checktest.NoAlerts(t, s)
}

func TestCommandKeepsItsSpacesAndIsCutAtFortyCharacters(t *testing.T) {
	long := "/usr/bin/java  -Xmx4g -jar /opt/app/really-long-name-éé.jar --flag"
	ps := parsePS(psHeader + "\n" + psLine(42, "1.0", "2.0", "Sl", long))
	if len(ps) != 1 {
		t.Fatalf("parsed %d", len(ps))
	}
	if want := string([]rune(long)[:40]); ps[0].cmd != want {
		t.Fatalf("cmd %q, want %q", ps[0].cmd, want)
	}
}

func TestMalformedPSLinesAreSkipped(t *testing.T) {
	ps := parsePS(psHeader + "\nshort line\nroot 1 x 0.1 1 1 ? S Jan01 0:00 init\n" + psLine(2, "0.0", "0.0", "S", "ok"))
	if len(ps) != 1 || ps[0].pid != "2" {
		t.Fatalf("parsed %+v", ps)
	}
}

func TestPSThatCannotRunSaysSo(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", "").Code(1).Stderr("ps: command not found")
	s := e.Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"Processes"}) {
		t.Fatalf("labels %v", got)
	}
	checktest.NoAlerts(t, s)
}

func TestSplitN(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want []string
	}{
		{"  a  b   c d  ", 3, []string{"a", "b", "c d  "}},
		{"a b", 3, []string{"a", "b"}},
		{"", 3, nil},
		{"a\tb c", 2, []string{"a", "b c"}},
	}
	for _, c := range cases {
		if got := splitN(c.in, c.n); !slices.Equal(got, c.want) {
			t.Errorf("splitN(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.ZombieCaution = 60
	if c.Validate() == nil {
		t.Fatal("zombie_caution above zombie_unhealthy accepted")
	}
}
