package rootkit

import (
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

// procReads makes successive ReadDir("/proc") calls return successive
// listings (the last one repeats), which is what it takes to test code that
// deliberately reads /proc twice.
type procReads struct {
	*checktest.Runner
	mu    sync.Mutex
	reads [][]string
}

func (p *procReads) ReadDir(path string) ([]fs.DirEntry, error) {
	if path != "/proc" {
		return p.Runner.ReadDir(path)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	names := p.reads[0]
	if len(p.reads) > 1 {
		p.reads = p.reads[1:]
	}
	out := make([]fs.DirEntry, 0, len(names)+1)
	for _, n := range append([]string{"self"}, names...) {
		out = append(out, dirEntry(n))
	}
	return out, nil
}

type dirEntry string

func (d dirEntry) Name() string               { return string(d) }
func (d dirEntry) IsDir() bool                { return true }
func (d dirEntry) Type() fs.FileMode          { return fs.ModeDir }
func (d dirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }

// newEnv scripts the /proc listings ("1 2 4242" per read). PID 4242 stands
// for a process that really exists, so the final /proc/<pid> re-check passes
// and only the comparison logic can reject it.
func newEnv(t *testing.T, reads ...string) *checktest.Env {
	e := checktest.NewEnv(t, Check{}, nil)
	pr := &procReads{Runner: e.Fake}
	for _, r := range reads {
		pr.reads = append(pr.reads, strings.Fields(r))
	}
	e.Runner = pr
	e.Fake.Dir("/proc/1").Dir("/proc/2").Dir("/proc/4242")
	return e
}

func TestProcessThatExitsMidCheckIsNotReportedAsHidden(t *testing.T) {
	// Present in the first /proc read and gone from the second: a process
	// that simply exited, not something hiding from ps.
	e := newEnv(t, "1 2 4242", "1 2")
	e.Fake.Expect("ps -eo pid", "1\n2")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if r := checktest.Rows(s)["Process Visibility"]; r.Status != check.OK || r.Value != "OK" {
		t.Fatalf("Process Visibility: %+v", r)
	}
}

func TestProcessThatStartsMidCheckIsNotReportedAsHidden(t *testing.T) {
	e := newEnv(t, "1 2", "1 2 4242")
	e.Fake.Expect("ps -eo pid", "1\n2")
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestProcessPsMissedOnceIsNotReportedAsHidden(t *testing.T) {
	// ps can miss a process that is being scheduled; the second run catches
	// it. Only a PID absent from BOTH ps runs counts as hidden.
	e := newEnv(t, "1 2 4242")
	e.Fake.Expect("ps -eo pid", "1\n2").Once()
	e.Fake.Expect("ps -eo pid", "1\n2\n4242").Once()
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestAPersistentlyHiddenProcessIsReported(t *testing.T) {
	e := newEnv(t, "1 2 4242")
	e.Fake.Expect("ps -eo pid", "1\n2")
	s := e.Run(Check{})
	if len(s.Alerts) != 1 || !strings.Contains(s.Alerts[0].Msg, "hidden process") || s.Alerts[0].Status != check.Caution {
		t.Fatalf("alerts: %+v", s.Alerts)
	}
	if r := checktest.Rows(s)["Hidden Processes"]; r.Value != "1 PID(s) in /proc not visible in ps: 4242" || r.Status != check.Caution {
		t.Fatalf("Hidden Processes: %+v", r)
	}
}

func TestPIDThatVanishedBeforeTheRecheckIsDropped(t *testing.T) {
	// Stable across both /proc reads but gone by the time it is verified.
	e := newEnv(t, "1 2 999999")
	e.Fake.Expect("ps -eo pid", "1\n2")
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestKernelPIDsAreNeverHidden(t *testing.T) {
	e := newEnv(t, "1 2 4242")
	e.Fake.Expect("ps -eo pid", "4242")
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestHiddenPIDsAreListedNumerically(t *testing.T) {
	e := newEnv(t, "1 2 100 25 3000")
	e.Fake.Dir("/proc/100").Dir("/proc/25").Dir("/proc/3000")
	e.Fake.Expect("ps -eo pid", "1\n2")
	r := checktest.Rows(e.Run(Check{}))["Hidden Processes"]
	if r.Value != "3 PID(s) in /proc not visible in ps: 25, 100, 3000" {
		t.Fatalf("Hidden Processes: %+v", r)
	}
}

func TestFailedPsIsNotAMassOfHiddenProcesses(t *testing.T) {
	e := newEnv(t, "1 2 4242")
	e.Fake.Expect("ps -eo pid", "").Code(1)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if r := checktest.Rows(s)["Process Visibility"]; r.Status != check.Info {
		t.Fatalf("Process Visibility: %+v", r)
	}
}

func TestRkhunterIsOptional(t *testing.T) {
	e := newEnv(t, "1")
	e.Fake.Expect("ps -eo pid", "1")
	s := e.Run(Check{})
	if r := checktest.Rows(s)["rkhunter"]; r.Value != "Not installed (optional)" || r.Status != check.Info {
		t.Fatalf("rkhunter row: %+v", r)
	}
	if len(s.MissingTools) != 1 || !s.MissingTools[0].Optional || s.MissingTools[0].Package("apt-get") != "rkhunter" {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
	if s.Status != check.Info {
		t.Fatalf("status %v", s.Status)
	}
}

func TestRkhunterWarningsAreCaution(t *testing.T) {
	e := newEnv(t, "1")
	e.Fake.Tool("rkhunter")
	e.Fake.Expect("ps -eo pid", "1")
	e.Fake.Expect("rkhunter --check --sk --rwo",
		"Warning: The command '/usr/bin/lwp-request' has been replaced by a script\n"+
			"  Checking for hidden files   [ Warning ]\nsome info line")
	s := e.Run(Check{})
	var rows []check.Row
	for _, r := range s.Rows {
		if r.Label == "rkhunter" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 2 || rows[1].Value != "Checking for hidden files   [ Warning ]" || rows[0].Status != check.Caution {
		t.Fatalf("rkhunter rows: %+v", rows)
	}
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"rkhunter warning"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestRkhunterClean(t *testing.T) {
	e := newEnv(t, "1")
	e.Fake.Tool("rkhunter")
	e.Fake.Expect("ps -eo pid", "1")
	s := e.Run(Check{})
	if r := checktest.Rows(s)["rkhunter"]; r.Value != "No warnings" || r.Status != check.OK {
		t.Fatalf("rkhunter row: %+v", r)
	}
	if len(s.MissingTools) != 0 {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
}

func TestSuspiciousPathIsUnhealthy(t *testing.T) {
	e := newEnv(t, "1")
	e.Fake.Expect("ps -eo pid", "1")
	e.Fake.File("/usr/bin/.sniffer", "")
	s := e.Run(Check{})
	if r := checktest.Rows(s)["Suspicious Path"]; r.Value != "/usr/bin/.sniffer" || r.Status != check.Unhealthy {
		t.Fatalf("row: %+v", r)
	}
	if len(s.Alerts) != 1 || s.Alerts[0] != (check.Alert{Status: check.Unhealthy, Msg: "Suspicious file/dir found: /usr/bin/.sniffer"}) {
		t.Fatalf("alerts: %+v", s.Alerts)
	}
	if _, ok := checktest.Rows(s)["Known Rootkit Paths"]; ok {
		t.Fatal("clean row alongside a finding")
	}
}

func TestNoSuspiciousPaths(t *testing.T) {
	e := newEnv(t, "1")
	e.Fake.Expect("ps -eo pid", "1")
	if r := checktest.Rows(e.Run(Check{}))["Known Rootkit Paths"]; r.Value != "None found" || r.Status != check.OK {
		t.Fatalf("row: %+v", r)
	}
}
