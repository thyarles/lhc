package ports

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

// ssOut renders socket rows the way `ss -tlnp` / `ss -ulnp` print them:
// a header, then State Recv-Q Send-Q Local Peer Process.
func ssOut(state string, rows ...[2]string) string {
	lines := []string{"State  Recv-Q Send-Q  Local Address:Port  Peer Address:Port Process"}
	for _, r := range rows {
		lines = append(lines, state+"  0  128  "+r[0]+"  0.0.0.0:*  "+r[1])
	}
	return strings.Join(lines, "\n")
}

func tcp(rows ...[2]string) string { return ssOut("LISTEN", rows...) }
func udp(rows ...[2]string) string { return ssOut("UNCONN", rows...) }

func newEnv(t *testing.T, cfg *Config) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	e.Fake.Tool("ss")
	return e
}

// listed is the full socket inventory: the unlabelled rows.
func listed(s *check.Section) []string {
	var out []string
	for _, r := range s.Rows {
		if r.Label == "" && r.Value != "" {
			out = append(out, r.Value)
		}
	}
	return out
}

func TestPortKeyIgnoresPidAndFd(t *testing.T) {
	cases := [][2]string{
		{`0.0.0.0:22 users:(("sshd",pid=101,fd=3))`, `0.0.0.0:22 users:(("sshd",pid=999,fd=3))`},
		{`127.0.0.1:5432 users:(("postgres",pid=7,fd=11))`, `127.0.0.1:5432 users:(("postgres",pid=8,fd=12))`},
	}
	for _, c := range cases {
		if a, b := portKey(c[0]), portKey(c[1]); a != b {
			t.Errorf("%q != %q", a, b)
		}
	}
	if got := portKey(`tcp  0.0.0.0:22   users:(("sshd",pid=101,fd=3))`); got != `tcp 0.0.0.0:22 users:(("sshd"))` {
		t.Errorf("portKey = %q", got)
	}
}

func TestPortKeyStillSeparatesDifferentPorts(t *testing.T) {
	if portKey(`0.0.0.0:22 users:(("sshd",pid=1,fd=3))`) == portKey(`0.0.0.0:23 users:(("sshd",pid=1,fd=3))`) {
		t.Fatal("ports 22 and 23 normalised to the same key")
	}
}

func TestPortKeyNoticesADifferentProcessOnTheSamePort(t *testing.T) {
	if portKey(`0.0.0.0:80 users:(("nginx",pid=1,fd=3))`) == portKey(`0.0.0.0:80 users:(("evil",pid=1,fd=3))`) {
		t.Fatal("a different process on port 80 went unnoticed")
	}
}

func TestRestartedServiceIsNotReportedAsANewPort(t *testing.T) {
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`tcp 0.0.0.0:22 users:(("sshd"))`})
	e.Fake.Expect("ss -tlnp", tcp([2]string{"0.0.0.0:22", `users:(("sshd",pid=999,fd=3))`}))
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["Port Changes"]; got.Status != check.OK || got.Value != "None since last run" {
		t.Fatalf("Port Changes row: %+v", got)
	}
}

func TestAGenuinelyNewPortStillAlerts(t *testing.T) {
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`tcp 0.0.0.0:22 users:(("sshd"))`})
	e.Fake.Expect("ss -tlnp", tcp(
		[2]string{"0.0.0.0:22", `users:(("sshd",pid=101,fd=3))`},
		[2]string{"0.0.0.0:4444", `users:(("nc",pid=555,fd=3))`},
	))
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"New listening port: tcp 0.0.0.0:4444"}) {
		t.Fatalf("alerts: %q", got)
	}
	if got := checktest.Rows(s)["NEW"]; got.Status != check.Caution || got.Value != `tcp 0.0.0.0:4444 users:(("nc"))` {
		t.Fatalf("NEW row: %+v", got)
	}
}

func TestAPortThatStoppedListeningIsInformational(t *testing.T) {
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`tcp 0.0.0.0:22 users:(("sshd"))`, `tcp 0.0.0.0:80 users:(("nginx"))`})
	e.Fake.Expect("ss -tlnp", tcp([2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`}))
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["GONE"]; got.Status != check.Info || !strings.Contains(got.Value, "0.0.0.0:80") {
		t.Fatalf("GONE row: %+v", got)
	}
}

func TestFirstRunRecordsABaselineWithoutAlerting(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("ss -tlnp", tcp([2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`}))
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["Baseline"].Value; got != "1 socket(s) recorded (first run)" {
		t.Fatalf("Baseline = %q", got)
	}
	var saved []string
	if !e.Loaded(t, stateKey, &saved) || !slices.Equal(saved, []string{`tcp 0.0.0.0:22 users:(("sshd"))`}) {
		t.Fatalf("saved = %q", saved)
	}
}

func TestEveryListeningSocketIsListed(t *testing.T) {
	// The socket inventory is never truncated: an "and N more" line hides
	// precisely what a reader opened the section to check.
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`tcp 0.0.0.0:1 users:(("x"))`})
	var rows [][2]string
	for p := 1000; p < 1040; p++ {
		rows = append(rows, [2]string{"0.0.0.0:" + strconv.Itoa(p), `users:(("svc",pid=1,fd=3))`})
	}
	e.Fake.Expect("ss -tlnp", tcp(rows...))
	s := e.Run(Check{})
	got := listed(s)
	if len(got) != 40 {
		t.Fatalf("only %d of 40 sockets listed", len(got))
	}
	if slices.Contains(checktest.Labels(s), "…") {
		t.Fatal("socket list was capped")
	}
	if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, "0.0.0.0:1039") }) {
		t.Fatal("the last socket must appear")
	}
}

func TestLoopbackClassification(t *testing.T) {
	cases := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:6444", true},
		{"127.0.0.53%lo:53", true}, // systemd-resolved, with a zone id
		{"[::1]:323", true},
		{"10.255.255.254:53", false},
		{"0.0.0.0:22", false},
		{"[::]:22", false},
		{"*:8472", false}, // a wildcard bind is the opposite of local
	}
	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.loopback {
			t.Errorf("isLoopback(%q) = %v, want %v", c.addr, got, c.loopback)
		}
	}
}

func TestHostPortSplitting(t *testing.T) {
	cases := []struct{ addr, host, port string }{
		{"127.0.0.1:6444", "127.0.0.1", "6444"},
		{"[::1]:323", "::1", "323"},
		{"[fe80::1%eth0]:80", "fe80::1%eth0", "80"},
		{"*:8472", "*", "8472"},
	}
	for _, c := range cases {
		if h, p := splitHostPort(c.addr); h != c.host || p != c.port {
			t.Errorf("splitHostPort(%q) = %q, %q; want %q, %q", c.addr, h, p, c.host, c.port)
		}
	}
}

func TestUDPSocketsAreIncluded(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("ss -tlnp", tcp([2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`}))
	e.Fake.Expect("ss -ulnp", udp([2]string{"0.0.0.0:161", `users:(("snmpd",pid=2,fd=4))`}))
	got := strings.Join(listed(e.Run(Check{})), " ")
	if !strings.Contains(got, "udp 0.0.0.0:161") {
		t.Fatalf("UDP sockets were previously invisible: %q", got)
	}
}

func TestLoopbackSocketsAreHiddenByDefault(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect("ss -tlnp", tcp(
		[2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`},
		[2]string{"127.0.0.1:6444", `users:(("k3s",pid=2,fd=4))`},
	))
	e.Fake.Expect("ss -ulnp", udp([2]string{"127.0.0.53%lo:53", `users:(("resolved",pid=3,fd=5))`}))
	s := e.Run(Check{})
	got := strings.Join(listed(s), " ")
	if !strings.Contains(got, "0.0.0.0:22") || strings.Contains(got, "6444") || strings.Contains(got, "127.0.0.53") {
		t.Fatalf("listed: %q", got)
	}
	if v := checktest.Rows(s)["Loopback-only Sockets"].Value; v != "2 hidden" {
		t.Fatalf("Loopback-only Sockets = %q", v)
	}
}

func TestListLocalShowsLoopbackSockets(t *testing.T) {
	e := newEnv(t, &Config{Toggle: check.On, ListLocal: true})
	e.Fake.Expect("ss -tlnp", tcp(
		[2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`},
		[2]string{"127.0.0.1:6444", `users:(("k3s",pid=2,fd=4))`},
	))
	s := e.Run(Check{})
	got := strings.Join(listed(s), " ")
	if !strings.Contains(got, "0.0.0.0:22") || !strings.Contains(got, "6444") {
		t.Fatalf("listed: %q", got)
	}
	if slices.Contains(checktest.Labels(s), "Loopback-only Sockets") {
		t.Fatal("hidden count shown although nothing is hidden")
	}
}

func TestChurningLoopbackPortsDoNotRaiseAlerts(t *testing.T) {
	// Applications open random high ports on 127.0.0.1 constantly.
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`tcp 0.0.0.0:22 users:(("sshd"))`})
	e.Fake.Expect("ss -tlnp", tcp(
		[2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`},
		[2]string{"127.0.0.1:46051", `users:(("MainThread",pid=1481,fd=22))`},
		[2]string{"127.0.0.1:39775", `users:(("MainThread",pid=1482,fd=23))`},
	))
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestANewExposedPortStillAlertsWithItsProtocol(t *testing.T) {
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`tcp 0.0.0.0:22 users:(("sshd"))`})
	e.Fake.Expect("ss -tlnp", tcp([2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`}))
	e.Fake.Expect("ss -ulnp", udp([2]string{"0.0.0.0:4444", `users:(("nc",pid=9,fd=3))`}))
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"New listening port: udp 0.0.0.0:4444"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestOldStateWithoutAProtocolPrefixRebaselinesQuietly(t *testing.T) {
	// Upgrading must not report every socket on the host as brand new.
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{`0.0.0.0:22 users:(("sshd",pid=101,fd=3))`}) // pre-UDP format
	e.Fake.Expect("ss -tlnp", tcp(
		[2]string{"0.0.0.0:22", `users:(("sshd",pid=1,fd=3))`},
		[2]string{"0.0.0.0:443", `users:(("nginx",pid=2,fd=4))`},
	))
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if !slices.ContainsFunc(s.Rows, func(r check.Row) bool { return strings.Contains(r.Value, "re-recorded") }) {
		t.Fatalf("no re-baseline row: %v", checktest.Labels(s))
	}
}

func TestEmptyPreviousBaselineIsNotAFormatChange(t *testing.T) {
	// A host that exposed nothing last time must not re-baseline forever.
	e := newEnv(t, nil)
	e.Seed(t, stateKey, []string{})
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Port Changes"].Value; got != "None since last run" {
		t.Fatalf("rows: %v", checktest.Labels(s))
	}
}

func TestSocketsWithoutAProcessColumn(t *testing.T) {
	// Without root, ss prints no process column at all.
	e := newEnv(t, nil)
	e.Fake.Expect("ss -tlnp", "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*")
	if got := listed(e.Run(Check{})); !slices.Equal(got, []string{"tcp 0.0.0.0:22"}) {
		t.Fatalf("listed: %q", got)
	}
}

func TestNetstatFallbackNormalisesThePID(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("netstat")
	e.Seed(t, stateKey, []string{"tcp 0.0.0.0:22 sshd", "udp 0.0.0.0:68 dhclient"})
	e.Fake.Expect("netstat -tlnp", `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN      4242/sshd`)
	e.Fake.Expect("netstat -ulnp", `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
udp        0      0 0.0.0.0:68              0.0.0.0:*                           777/dhclient`)
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := listed(s); !slices.Equal(got, []string{"tcp 0.0.0.0:22 sshd", "udp 0.0.0.0:68 dhclient"}) {
		t.Fatalf("listed: %q", got)
	}
	if len(s.MissingTools) != 1 || !s.MissingTools[0].Optional || s.MissingTools[0].Package("apt-get") != "iproute2" {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
}

func TestNeitherSSNorNetstat(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	if got := checktest.Rows(s)["ss / netstat"].Value; got != "Neither available" {
		t.Fatalf("rows: %v", checktest.Labels(s))
	}
	if len(s.MissingTools) != 1 || s.MissingTools[0].Optional || s.MissingTools[0].Package("dnf") != "iproute" {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
	if e.Store.Has(stateKey) {
		t.Fatal("baseline saved without any data")
	}
}
