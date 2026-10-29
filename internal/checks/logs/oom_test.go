package logs

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

const (
	cid    = "4f1d0c3b9a8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a392817060000"
	uid    = "1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	uidSD  = "1a2b3c4d_5e6f_7a8b_9c0d_1e2f3a4b5c6d" // the systemd cgroup driver's spelling
	slice  = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + uidSD + ".slice/cri-containerd-" + cid + ".scope"
	podAPI = `{"status":{"labels":{"io.kubernetes.pod.name":"api-6b7c9d-x2k4p","io.kubernetes.pod.namespace":"team-a","io.kubernetes.container.name":"app"}}}`
)

// rhel8Kill is one container-limit kill on a RHEL 8 rke2 worker, as the
// journal prints it: four lines match the pattern, the reaper's does not.
func rhel8Kill(clock, pid string) string {
	return strings.Join([]string{
		"Jan 05 " + clock + " node1 kernel: java invoked oom-killer: gfp_mask=0x6000c0(GFP_KERNEL), nodemask=(null), order=0, oom_score_adj=985",
		"Jan 05 " + clock + " node1 kernel:  oom_kill_process.cold.33+0xb/0x10",
		"Jan 05 " + clock + " node1 kernel: oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=cri-containerd-" + cid + ".scope,mems_allowed=0,oom_memcg=" + slice + ",task_memcg=" + slice + ",task=java,pid=" + pid + ",uid=1000",
		"Jan 05 " + clock + " node1 kernel: Memory cgroup out of memory: Killed process " + pid + " (java) total-vm:8123456kB, anon-rss:524000kB, file-rss:21000kB, shmem-rss:0kB, UID:1000 pgtables:1500kB oom_score_adj:985",
		"Jan 05 " + clock + " node1 kernel: oom_reaper: reaped process " + pid + " (java), now anon-rss:0kB, file-rss:0kB, shmem-rss:0kB",
	}, "\n")
}

const rhel8HostKill = `Jan 05 06:01:02 node1 kernel: postgres invoked oom-killer: gfp_mask=0x6200ca(GFP_HIGHUSER_MOVABLE), order=0, oom_score_adj=0
Jan 05 06:01:02 node1 kernel:  oom_kill_process.cold.33+0xb/0x10
Jan 05 06:01:02 node1 kernel: oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/system.slice/postgresql.service,task=postgres,pid=4321,uid=26
Jan 05 06:01:02 node1 kernel: Out of memory: Killed process 4321 (postgres) total-vm:2123456kB, anon-rss:1824000kB, file-rss:0kB, shmem-rss:0kB, UID:26`

// The 3.10 kernel (RHEL 7): the cgroup comes on a "Task in" line, with
// no pid, and the scope on the "or sacrifice child" line before the kill.
const rhel7Kills = `Jan  5 02:10:01 node2 kernel: java invoked oom-killer: gfp_mask=0xd0, order=0, oom_score_adj=999
Jan  5 02:10:01 node2 kernel: [<ffffffff9a1bd4f4>] oom_kill_process+0x254/0x3e0
Jan  5 02:10:01 node2 kernel: Task in /kubepods/burstable/pod` + uid + `/` + cid + ` killed as a result of limit of /kubepods/burstable/pod` + uid + `/` + cid + `
Jan  5 02:10:01 node2 kernel: Memory cgroup out of memory: Kill process 23456 (java) score 1990 or sacrifice child
Jan  5 02:10:01 node2 kernel: Killed process 23456 (java) total-vm:4123456kB, anon-rss:262000kB, file-rss:12000kB, shmem-rss:0kB
Jan  5 04:20:11 node2 kernel: mysqld invoked oom-killer: gfp_mask=0x201da, order=0, oom_score_adj=0
Jan  5 04:20:11 node2 kernel: [<ffffffff9a1bd4f4>] oom_kill_process+0x254/0x3e0
Jan  5 04:20:11 node2 kernel: Out of memory: Kill process 999 (mysqld) score 900 or sacrifice child
Jan  5 04:20:11 node2 kernel: Killed process 999 (mysqld) total-vm:3123456kB, anon-rss:2900000kB, file-rss:0kB, shmem-rss:0kB`

func oomEnv(t *testing.T, journal string) *checktest.Env {
	e := logsEnv(t, nil, nil)
	e.Fake.Expect("Killed process", journal)
	return e
}

func killRows(s *check.Section) []check.Row {
	i := slices.Index(checktest.Labels(s), "Kills today")
	if i < 0 {
		return nil
	}
	var out []check.Row
	for _, r := range s.Rows[i+1:] {
		if r.Label != "" {
			break
		}
		out = append(out, r)
	}
	return out
}

func TestOOMCountsKillsNotLines(t *testing.T) {
	e := oomEnv(t, rhel8Kill("03:12:40", "12345")+"\n"+rhel8Kill("03:15:02", "12399"))
	e.Fake.Tool("crictl").Expect("inspect "+cid, podAPI)
	s := e.Run(Check{})

	if got := s.AlertMsgs(); !slices.Equal(got, []string{"OOM Killer: 2 kill(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
	if r := checktest.Rows(s)[oomLabel]; r.Value != "2 kill(s) today" || r.Status != check.Caution {
		t.Fatalf("row: %+v", r)
	}
	rows := killRows(s)
	if len(rows) != 1 || rows[0].Value != "2× java · pod team-a/api-6b7c9d-x2k4p (container app) · container limit · last 03:15" || rows[0].Status != check.Caution {
		t.Fatalf("kill rows: %+v", rows)
	}
	want := check.PodRef{UID: uid, Namespace: "team-a", Name: "api-6b7c9d-x2k4p", Container: "app", Process: "java", Reason: "oom_killed", Count: 2}
	if len(s.Pods) != 1 || s.Pods[0].Last.Format("15:04:05") != "03:15:02" {
		t.Fatalf("pods: %+v", s.Pods)
	}
	got := s.Pods[0]
	got.Last = want.Last
	if got != want {
		t.Fatalf("pod: %+v", got)
	}
	// One crictl call for the one container, however many kills.
	n := 0
	for _, c := range e.Fake.Calls() {
		if strings.Contains(c, "crictl") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("crictl calls: %q", e.Fake.Calls())
	}
}

func TestAHostOutOfMemoryIsUnhealthy(t *testing.T) {
	s := oomEnv(t, rhel8Kill("03:12:40", "12345")+"\n"+rhel8HostKill).Run(Check{})
	if len(s.Alerts) != 1 || s.Alerts[0].Status != check.Unhealthy || s.Alerts[0].Msg != "OOM Killer: 2 kill(s) today" {
		t.Fatalf("alerts: %+v", s.Alerts)
	}
	rows := killRows(s)
	if len(rows) != 2 || rows[0].Value != "1× postgres · host · last 06:01" || rows[0].Status != check.Unhealthy {
		t.Fatalf("kill rows: %+v", rows)
	}
	// Only container kills go to the hub.
	if len(s.Pods) != 1 || s.Pods[0].Process != "java" {
		t.Fatalf("pods: %+v", s.Pods)
	}
}

func TestTheRHEL7KernelIsRead(t *testing.T) {
	kills := parseKills(todaysOOM(strings.Split(rhel7Kills, "\n"), checktest.Now), checktest.Now)
	if len(kills) != 2 {
		t.Fatalf("kills: %+v", kills)
	}
	if k := kills[0]; k.pid != "23456" || k.proc != "java" || k.uid != uid || k.cid != cid || k.scope != scopeContainer || k.at.Format("15:04") != "02:10" {
		t.Fatalf("container kill: %+v", k)
	}
	if k := kills[1]; k.pid != "999" || k.proc != "mysqld" || k.uid != "" || k.scope != scopeHost {
		t.Fatalf("host kill: %+v", k)
	}
}

func TestAKilledContainerIsFoundByItsPodUID(t *testing.T) {
	// The container is gone (crictl inspect fails), the pod still runs:
	// the systemd driver's UID is asked for with dashes.
	e := oomEnv(t, rhel8Kill("03:12:40", "12345"))
	e.Fake.Tool("crictl").Expect("inspect "+cid, "").Code(1)
	e.Fake.Expect("pods --label io.kubernetes.pod.uid="+uid+" -o json", `{"items":[{"metadata":{"name":"api-6b7c9d-x2k4p","uid":"`+uid+`","namespace":"team-a"}}]}`)
	s := e.Run(Check{})
	if rows := killRows(s); len(rows) != 1 || rows[0].Value != "1× java · pod team-a/api-6b7c9d-x2k4p · container limit · last 03:12" {
		t.Fatalf("kill rows: %+v", rows)
	}
	if len(s.Pods) != 1 || s.Pods[0].Name != "api-6b7c9d-x2k4p" || s.Pods[0].Container != "" {
		t.Fatalf("pods: %+v", s.Pods)
	}
}

func TestWithoutCrictlTheUIDIsShown(t *testing.T) {
	s := oomEnv(t, rhel8Kill("03:12:40", "12345")).Run(Check{})
	if s.Err != nil {
		t.Fatal(s.Err)
	}
	if rows := killRows(s); len(rows) != 1 || rows[0].Value != "1× java · pod UID 1a2b3c4d-… (not found) · container limit · last 03:12" {
		t.Fatalf("kill rows: %+v", rows)
	}
	// The collector can still look the UID up in the cluster.
	if len(s.Pods) != 1 || s.Pods[0].UID != uid || s.Pods[0].Name != "" {
		t.Fatalf("pods: %+v", s.Pods)
	}
}

func TestAnUnknownOOMWordingFallsBackToTheLineCount(t *testing.T) {
	s := oomEnv(t, "Jan 05 03:00:01 node1 kernel: some-driver: out of memory, dropping frame\n"+
		"Jan 05 03:00:02 node1 kernel: some-driver: out of memory, dropping frame").Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"OOM Killer: 2 occurrence(s) today"}) {
		t.Fatalf("alerts: %q", got)
	}
	if !slices.Contains(checktest.Labels(s), "Recent entries") || len(s.Pods) != 0 {
		t.Fatalf("labels: %q", checktest.Labels(s))
	}
}

func TestOOMLinesThatAreNotAKillOfTodayDoNotAlert(t *testing.T) {
	for name, journal := range map[string]string{
		"the reaper alone": "Jan 05 03:12:40 node1 kernel: oom_reaper: reaped process 12345 (java), now anon-rss:0kB, file-rss:0kB, shmem-rss:0kB",
		"yesterday":        strings.ReplaceAll(rhel8Kill("23:59:01", "12345"), "Jan 05", "Jan 04"),
		"nothing":          "",
	} {
		t.Run(name, func(t *testing.T) {
			s := oomEnv(t, journal).Run(Check{})
			checktest.NoAlerts(t, s)
			if r := checktest.Rows(s)[oomLabel]; r.Value != "0 kill(s) today" || r.Status != check.OK {
				t.Fatalf("row: %+v", r)
			}
		})
	}
}

func TestOnlyTheFirstFiveKilledProcessesAreListed(t *testing.T) {
	var lines []string
	for i := range 7 {
		lines = append(lines, fmt.Sprintf("Jan 05 03:%02d:00 node1 kernel: Out of memory: Killed process %d (proc%d) total-vm:1kB", i, 100+i, i))
	}
	s := oomEnv(t, strings.Join(lines, "\n")).Run(Check{})
	rows := killRows(s)
	if len(rows) != 6 || rows[5].Value != "… 2 more" || rows[0].Value != "1× proc6 · host · last 03:06" {
		t.Fatalf("kill rows: %+v", rows)
	}
}
