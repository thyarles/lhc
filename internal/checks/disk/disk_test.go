package disk

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checktest"
)

const header = "Filesystem     1024-blocks      Used Available Capacity Mounted on"

func df(lines ...string) string { return strings.Join(append([]string{header}, lines...), "\n") }

func newEnv(t *testing.T, cfg *Config, dfOut string) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	e.Fake.Expect("df -Pk", dfOut)
	return e
}

func TestPodSandboxShmMountsAreNotDisks(t *testing.T) {
	// The reported symptom. One 64MB shm tmpfs per pod, always 0%, with a new
	// identity every time a pod restarts, so the delta state churned forever
	// too. They survived the old filter because it looked for 'tmpfs' and a
	// sandbox shm mount's device is literally 'shm'.
	lines := []string{"/dev/sda1 1000000 400000 600000 40% /"}
	for _, h := range []string{"f0fa", "62db", "37fd"} {
		lines = append(lines, fmt.Sprintf("shm 65536 0 65536 0%% /run/k3s/containerd/io.containerd.grpc.v1.cri/sandboxes/%s/shm",
			strings.Repeat(h, 16)))
	}
	s := newEnv(t, nil, df(lines...)).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("labels: %v", got)
	}
}

func TestKubeletBindMountsDoNotMultiplyOneFilesystem(t *testing.T) {
	// The dangerous half. A subPath or local-path PVC is a bind mount of the
	// root filesystem, so df reports it with the ROOT device and numbers.
	// Forty such pods once meant forty identical rows and, the moment /
	// crossed 90%, forty identical alerts each with its own fingerprint.
	lines := []string{"/dev/sda1 1000000 910000 90000 91% /"}
	for i := range 40 {
		lines = append(lines, fmt.Sprintf("/dev/sda1 1000000 910000 90000 91%% /var/lib/kubelet/pods/uid-%d/volume-subpaths/data/app/0", i))
	}
	s := newEnv(t, nil, df(lines...)).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("labels: %v", got)
	}
	if len(s.Alerts) != 1 {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
}

func TestABindMountOutsideTheGlobListIsStillDeduplicated(t *testing.T) {
	// Globs only catch paths someone anticipated. Identical device, size,
	// used and free means the same filesystem whatever it is mounted at.
	s := newEnv(t, nil, df(
		"/dev/sda1 1000000 910000 90000 91% /some/vendor/bind/mount",
		"/dev/sda1 1000000 910000 90000 91% /",
	)).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/", "Hidden Mounts"}) {
		t.Fatalf("labels: %v (shortest mount must win, wherever it is listed)", got)
	}
	if r := checktest.Rows(s)["Hidden Mounts"]; r.Status != check.Info ||
		r.Value != "1 bind/container mount(s) on filesystems already listed" {
		t.Fatalf("Hidden Mounts row: %+v", r)
	}
	if len(s.Alerts) != 1 {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
}

func TestADedicatedFilesystemUnderAnIgnoredPrefixIsStillReported(t *testing.T) {
	// The trailing '/*' is load-bearing: /var/lib/docker/* skips per-container
	// mounts while a real filesystem mounted AT /var/lib/docker is kept.
	s := newEnv(t, nil, df(
		"/dev/sda1 1000000 400000 600000 40% /",
		"/dev/sdb1 5000000 4800000 200000 96% /var/lib/docker",
		"/dev/sdb1 5000000 4800000 200000 96% /var/lib/docker/overlay2/abc/merged",
	)).Run(Check{})
	row, ok := checktest.Rows(s)["/var/lib/docker"]
	if !ok || row.Status != check.Unhealthy {
		t.Fatalf("/var/lib/docker row: %+v (labels %v)", row, checktest.Labels(s))
	}
	if !slices.Equal(s.AlertMsgs(), []string{"Disk /var/lib/docker at 96%"}) {
		t.Fatalf("alerts: %q", s.AlertMsgs())
	}
}

func TestDiskIgnoreIsAdditiveNotAReplacement(t *testing.T) {
	// A host that needs one extra path must not silently lose the built-in
	// container list.
	cfg := Check{}.Defaults().(*Config)
	cfg.Ignore = []string{"/mnt/backup-scratch/*"}
	s := newEnv(t, cfg, df(
		"/dev/sda1 1000000 400000 600000 40% /",
		"/dev/sdc1 900 800 100 89% /mnt/backup-scratch/tmp",
		"shm 65536 0 65536 0% /run/k3s/containerd/x/shm",
	)).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("labels: %v", got)
	}
}

func TestDiskRowsCarryAMeter(t *testing.T) {
	s := newEnv(t, nil, df("/dev/sda1 1000000 910000 90000 91% /var")).Run(Check{})
	row := checktest.Rows(s)["/var"]
	if row.Meter == nil || *row.Meter != 91 {
		t.Fatalf("meter: %v", row.Meter)
	}
}

func TestDiskDeltaAppearsOnTheSecondRun(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("df -Pk", df("/dev/sda1 1000000 880000 120000 88% /var")).Once()
	e.Fake.Expect("df -Pk", df("/dev/sda1 1000000 910000 90000 91% /var"))
	e.Run(Check{}) // records 88.0
	row := checktest.Rows(e.Run(Check{}))["/var"]
	if row.Delta != "+3.0 pts since last run" {
		t.Fatalf("delta %q", row.Delta)
	}
}

func TestFirstEverRunShowsNoDelta(t *testing.T) {
	s := newEnv(t, nil, df("/dev/sda1 1000000 910000 90000 91% /var")).Run(Check{})
	if d := checktest.Rows(s)["/var"].Delta; d != "" {
		t.Fatalf("delta %q", d)
	}
}

func TestAnUnchangedDiskShowsNoDeltaAtAll(t *testing.T) {
	// Silence is the message. "no change since last run" on every mount cost
	// a line each and told the reader nothing.
	e := newEnv(t, nil, df("/dev/sda1 1000000 910000 90000 91% /var"))
	e.Run(Check{})
	if d := checktest.Rows(e.Run(Check{}))["/var"].Delta; d != "" {
		t.Fatalf("delta %q", d)
	}
}

func TestThresholdsAndAlerts(t *testing.T) {
	cases := []struct {
		pct   string
		want  check.Status
		alert string
	}{
		{"89%", check.OK, ""},
		// Unlike RAM, a filling disk alerts at CAUTION too: it only gets worse.
		{"90%", check.Caution, "Disk /data at 90%"},
		{"95%", check.Unhealthy, "Disk /data at 95%"},
	}
	for _, c := range cases {
		s := newEnv(t, nil, df("/dev/sdb1 1000 900 100 "+c.pct+" /data")).Run(Check{})
		if got := checktest.Rows(s)["/data"].Status; got != c.want {
			t.Errorf("%s: status %v, want %v", c.pct, got, c.want)
		}
		got := s.AlertMsgs()
		switch {
		case c.alert == "" && len(got) != 0:
			t.Errorf("%s: unexpected alerts %q", c.pct, got)
		case c.alert != "" && (!slices.Equal(got, []string{c.alert}) || s.Alerts[0].Status != c.want):
			t.Errorf("%s: alerts %+v, want %q at %v", c.pct, s.Alerts, c.alert, c.want)
		}
	}
}

func TestValueFormat(t *testing.T) {
	s := newEnv(t, nil, df("/dev/sda1 10485760 4194304 6291456 40% /")).Run(Check{})
	if v := checktest.Rows(s)["/"].Value; v != "40.0% used  (4 GB of 10 GB, 6 GB free)" {
		t.Fatalf("value %q", v)
	}
}

func TestStateRecordsEveryListedMount(t *testing.T) {
	e := newEnv(t, nil, df(
		"/dev/sda1 1000000 400000 600000 40% /",
		"/dev/sda1 1000000 400000 600000 40% /bind",
		"/dev/sdb1 1000 500 500 50% /data",
	))
	e.Run(Check{})
	var saved map[string]float64
	if !e.Loaded(t, stateKey, &saved) || len(saved) != 2 || saved["/"] != 40 || saved["/data"] != 50 {
		t.Fatalf("saved %v", saved)
	}
}

func TestPseudoFilesystemsAreSkipped(t *testing.T) {
	s := newEnv(t, nil, df(
		"/dev/sda1 1000000 400000 600000 40% /",
		"tmpfs 1000 0 1000 0% /run",
		"devtmpfs 1000 0 1000 0% /dev",
		"udev 1000 0 1000 0% /dev",
		"overlay 1000 500 500 50% /var/lib/docker/overlay2/x/merged2",
		"overlay 1000 500 500 50% /merged",
		"/dev/loop0 1000 1000 0 100% /snap/core/123",
		"nsfs 0 0 0 - /run/netns/cni-1",
		"shm 65536 0 65536 0% /dev/shm",
	)).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("labels: %v", got)
	}
	checktest.NoAlerts(t, s)
}

func TestAMountPointWithSpacesKeepsItsName(t *testing.T) {
	s := newEnv(t, nil, df("/dev/sdb1 1000 500 500 50% /mnt/My Disk")).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/mnt/My Disk"}) {
		t.Fatalf("labels: %v", got)
	}
}

func TestMalformedLinesAreSkipped(t *testing.T) {
	s := newEnv(t, nil, df(
		"/dev/sda1 1000000 400000 600000 40% /",
		"//server/share",
		"/dev/sdb1 - - - - /broken",
	)).Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("labels: %v", got)
	}
}

func TestDfFailingOnOneMountStillReportsTheRest(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Expect("df -Pk", df("/dev/sda1 1000000 400000 600000 40% /")).Code(1).
		Stderr("df: /mnt/nfs: Stale file handle")
	if got := checktest.Labels(e.Run(Check{})); !slices.Equal(got, []string{"/"}) {
		t.Fatalf("labels: %v", got)
	}
}

func TestDedupeKeepsOrderAndTheShortestPath(t *testing.T) {
	rows := []mount{
		{"/dev/a", 1, 1, 0, 100, "/x/y"},
		{"/dev/b", 2, 1, 1, 50, "/data"},
		{"/dev/a", 1, 1, 0, 100, "/x"},
		{"/dev/a", 1, 1, 0, 100, "/x/z"},
		{"/dev/a", 9, 1, 8, 11, "/other"}, // same device, different numbers: a different fact
	}
	kept, hidden := dedupe(rows)
	var paths []string
	for _, r := range kept {
		paths = append(paths, r.path)
	}
	if !slices.Equal(paths, []string{"/data", "/x", "/other"}) || hidden != 2 {
		t.Fatalf("kept %v hidden %d", paths, hidden)
	}
}

func TestGlobFollowsPythonFnmatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		// '*' crosses '/': one trailing star covers a whole subtree.
		{"/var/lib/kubelet/pods/*", "/var/lib/kubelet/pods/uid/volume-subpaths/a/0", true},
		{"/var/lib/docker/*", "/var/lib/docker", false},
		{"/var/lib/docker/*", "/var/lib/docker/", true},
		{"/snap/*", "/snapshots", false},
		{"*", "/anything/at/all", true},
		{"/mnt/*/data", "/mnt/a/b/data", true},
		{"/mnt/**", "/mnt/a", true},
		{"/disk?", "/disk1", true},
		{"/disk?", "/disk10", false},
		{"/disk[0-9]", "/disk7", true},
		{"/disk[!0-9]", "/disk7", false},
		{"/disk[!0-9]", "/diskx", true},
		{"/d[]]", "/d]", true},
		{"/d[!]]", "/dx", true},
		{"/d[^a]", "/d^", true}, // '^' is literal in fnmatch, not negation
		{"/d[^a]", "/db", false},
		{"/d[", "/d[", true}, // an unclosed '[' is literal
		{"/d.x", "/dax", false},
		{"/a+b(c)", "/a+b(c)", true},
		{"/x/*", "/x/a\nb", true}, // DOTALL, as fnmatch
		{"/x", "/x/", false},      // anchored at both ends
	}
	for _, c := range cases {
		re, err := compileGlob(c.pat)
		if err != nil {
			t.Errorf("compileGlob(%q): %v", c.pat, err)
			continue
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%q vs %q = %v, want %v (re %s)", c.pat, c.path, got, c.want, re)
		}
	}
}

func TestDefaultIgnoreGlobsAllCompile(t *testing.T) {
	if got := ignoreSet(nil, func(g string, err error) { t.Errorf("%s: %v", g, err) }); len(got) != len(DefaultIgnore) {
		t.Fatalf("%d of %d compiled", len(got), len(DefaultIgnore))
	}
}

func TestBlankExtrasAreIgnored(t *testing.T) {
	if got := ignoreSet([]string{"", "  ", " /x/* "}, nil); len(got) != len(DefaultIgnore)+1 {
		t.Fatalf("%d globs", len(got))
	}
}

func TestFmtBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1 KB", 1 << 40: "1 TB", 1 << 50: "1 PB"}
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
	c = Check{}.Defaults().(*Config)
	c.Ignore = []string{"/ok/*", "/bad[z-a]"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "/bad[z-a]") {
		t.Fatalf("bad glob: %v", err)
	}
}

func TestTheDiskFactIsTheFullestListedMount(t *testing.T) {
	e := newEnv(t, nil, df(
		"/dev/sda1 10000000 4100000 5900000 41% /",
		"/dev/sdb1 10000000 7100000 2900000 71% /var",
		"/dev/sdc1 10000000 2000000 8000000 20% /home",
		// Ignored container storage never wins, however full.
		"/dev/sdd1 10000000 9900000 100000 99% /var/lib/docker/overlay2/abc",
	))
	got := checktest.Facts(e.Run(Check{}))["disk_max"]
	if got != (check.Fact{Key: "disk_max", Value: "71% /var", Status: check.OK}) {
		t.Fatalf("disk_max fact = %+v", got)
	}
}

func TestNoListedMountNoDiskFact(t *testing.T) {
	if _, ok := checktest.Facts(newEnv(t, nil, df()).Run(Check{}))["disk_max"]; ok {
		t.Fatal("a fact without a filesystem")
	}
}
