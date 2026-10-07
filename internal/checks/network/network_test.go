package network

import (
	"slices"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999     100    0    0    0     0          0         0  9999999     100    0    0    0     0       0          0
  eth0: 1073741824  2000    0    0    0     0          0         0 5242880    1500    0    0    0     0       0          0
docker0:   2048       10    0    0    0     0          0         0     512       5    0    0    0     0       0          0
`

const ssOut = `Total: 412
TCP:   37 (estab 12, closed 14, orphaned 0, timewait 9)

Transport Total     IP        IPv6
RAW	  1         0         1
UDP	  9         6         3
TCP	  23        17        6`

func TestInterfacesSkipLoopback(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.File("/proc/net/dev", netDev)
	s := e.Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"eth0", "docker0"}) {
		t.Fatalf("labels %v", got)
	}
	rows := checktest.Rows(s)
	if rows["eth0"].Value != "RX 1 GB  TX 5 MB" || rows["eth0"].Status != check.Info {
		t.Fatalf("eth0 %+v", rows["eth0"])
	}
	if rows["docker0"].Value != "RX 2 KB  TX 512 B" {
		t.Fatalf("docker0 %+v", rows["docker0"])
	}
	checktest.NoAlerts(t, s)
}

func TestCounterGluedToTheNameStillParses(t *testing.T) {
	got := parseNetDev([]byte("h1\nh2\n  eth1:123456789 1 0 0 0 0 0 0 4096 1 0 0 0 0 0 0\n"))
	if len(got) != 1 || got[0] != (iface{"eth1", 123456789, 4096}) {
		t.Fatalf("parsed %+v", got)
	}
}

func TestMalformedNetDevLinesAreSkipped(t *testing.T) {
	got := parseNetDev([]byte("h1\nh2\nnocolon 1 2 3\n eth0: 1 2 3\n eth1: x 0 0 0 0 0 0 0 1\n"))
	if len(got) != 0 {
		t.Fatalf("parsed %+v", got)
	}
	if parseNetDev(nil) != nil {
		t.Fatal("empty input parsed")
	}
}

func TestConnectionSummaryIsTheHeadOfSS(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("ss")
	e.Fake.Expect("ss -s", ssOut)
	s := e.Run(Check{})
	var got []string
	for _, r := range s.Rows {
		if r.Separator {
			got = append(got, "--"+r.Label)
			continue
		}
		got = append(got, r.Label+"|"+r.Value)
	}
	want := []string{
		"--Connection Summary",
		"|Total: 412",
		"|TCP:   37 (estab 12, closed 14, orphaned 0, timewait 9)",
		"|Transport Total     IP        IPv6",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows %q", got)
	}
	if len(s.MissingTools) != 0 {
		t.Fatalf("missing tools %+v", s.MissingTools)
	}
}

func TestNoSSOutputNoSeparator(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("ss")
	if s := e.Run(Check{}); len(s.Rows) != 0 {
		t.Fatalf("rows %+v", s.Rows)
	}
}

func TestWithoutSSAsksForIproute(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	s := e.Run(Check{})
	if e.Fake.Ran("ss") {
		t.Fatal("ran ss although it is not installed")
	}
	if len(s.MissingTools) != 1 || s.MissingTools[0].Package("dnf") != "iproute" ||
		s.MissingTools[0].Package("apt-get") != "iproute2" || s.MissingTools[0].Package("zypper") != "iproute2" {
		t.Fatalf("missing tools %+v", s.MissingTools)
	}
}

func TestFmtBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1 KB", 1 << 40: "1 TB", 5 << 50: "5 PB"}
	for n, want := range cases {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
