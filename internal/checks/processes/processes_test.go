package processes

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
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
	if row.Status != check.Caution || !strings.HasSuffix(row.Detail, "Thresholds: caution ≥ 2, unhealthy ≥ 3") {
		t.Fatalf("row %+v", row)
	}
}

func TestZombiesWithStatModifiersAreCounted(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", strings.Join([]string{
		psHeader,
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

// An rke2 worker had 1153 zombies, nearly all left by a Go service running
// as its container's PID 1. The count alone left the reader asking whether
// only a reboot would clear them.
func TestZombieParentsAndTheRemedyAreNamed(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", withZombies(60))
	e.Fake.Expect("ps -e -o ppid=,stat=", strings.Repeat("3333254 Zs\n", 55)+strings.Repeat("880 Z\n", 5)+"1 Ss\n3333254 S\n")
	e.Fake.File("/proc/3333254/comm", "consumer\n")
	e.Fake.File("/proc/3333254/status", "Name:\tconsumer\nNSpid:\t3333254\t1\n")
	e.Fake.File("/proc/880/comm", "bash\n")
	e.Fake.File("/proc/880/status", "Name:\tbash\nNSpid:\t880\n")
	s := e.Run(Check{})

	rows := checktest.Rows(s)
	z := rows["Zombie Processes"]
	if z.Value != "60" || z.Status != check.Unhealthy ||
		!strings.Contains(z.Detail, "Restarting the parent's container clears them; no reboot needed.") ||
		!strings.Contains(z.Detail, "tini") {
		t.Fatalf("zombie row %+v", z)
	}
	if got := rows["Parent PID 3333254"].Value; got != "55 · consumer (container PID 1)" {
		t.Fatalf("first parent %q", got)
	}
	if got := rows["Parent PID 880"].Value; got != "5 · bash" {
		t.Fatalf("second parent %q", got)
	}
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"60 zombie processes, most under consumer"}) {
		t.Fatalf("alerts %q", got)
	}
}

// 12 of 20 rke2 nodes had zombies under s3driver. Which pod that was took a
// crictl loop by hand on each node; the hub digest shows only the alert.
func TestTheZombieParentsPodIsNamed(t *testing.T) {
	const id = "4f1d0c3b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a392817060000"
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("crictl").Socket("/run/k3s/containerd/containerd.sock")
	e.Fake.Expect("ps aux", withZombies(2304))
	e.Fake.Expect("ps -e -o ppid=,stat=", strings.Repeat("8674 Z\n", 2304))
	e.Fake.File("/proc/8674/comm", "s3driver\n")
	e.Fake.File("/proc/8674/status", "Name:\ts3driver\nNSpid:\t8674\t1\n")
	e.Fake.File("/proc/8674/cgroup", "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod1a2b3c4d_5e6f_7a8b_9c0d_1e2f3a4b5c6d.slice/cri-containerd-"+id+".scope\n")
	e.Fake.Expect("--runtime-endpoint unix:///run/k3s/containerd/containerd.sock inspect "+id,
		`{"status":{"id":"`+id+`","labels":{"io.kubernetes.container.name":"csi-s3","io.kubernetes.pod.name":"csi-s3-66l84","io.kubernetes.pod.namespace":"infra-datashim"}}}`)
	s := e.Run(Check{})

	if got := checktest.Rows(s)["Parent PID 8674"].Value; got != "2304 · s3driver (container PID 1) · pod infra-datashim/csi-s3-66l84" {
		t.Fatalf("parent %q", got)
	}
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"2304 zombie processes, most under infra-datashim/csi-s3-66l84"}) {
		t.Fatalf("alerts %q", got)
	}
}

func TestAParentWithNoKnownCommandIsNotNamed(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", withZombies(12))
	e.Fake.Expect("ps -e -o ppid=,stat=", strings.Repeat("4242 Z\n", 12))
	if got := e.Run(Check{}).AlertMsgs(); !slices.Equal(got, []string{"12 zombie processes"}) {
		t.Fatalf("alerts %q", got)
	}
}

func TestZombiesOnTheHostNeedNoContainerAdvice(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", withZombies(12))
	e.Fake.Expect("ps -e -o ppid=,stat=", strings.Repeat("4242 Z\n", 12))
	e.Fake.File("/proc/4242/comm", "cron\n")
	s := e.Run(Check{})
	z := checktest.Rows(s)["Zombie Processes"]
	if !strings.Contains(z.Detail, "Restarting the parent clears them; no reboot needed.") || strings.Contains(z.Detail, "tini") {
		t.Fatalf("zombie row %+v", z)
	}
	if got := checktest.Rows(s)["Parent PID 4242"].Value; got != "12 · cron" {
		t.Fatalf("parent %q", got)
	}
}

func TestAFewZombiesDoNotLookForParents(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("ps aux", withZombies(3))
	e.Run(Check{})
	if slices.ContainsFunc(e.Fake.Calls(), func(c string) bool { return strings.Contains(c, "ppid=") }) {
		t.Fatal("looked up parents for zombies below the caution threshold")
	}
}
