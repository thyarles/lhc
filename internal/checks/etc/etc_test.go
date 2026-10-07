package etc

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

// newEnv fakes find and stat: every file reports the same mtime, given as
// epoch seconds, the way the Python tests faked both stat calls.
func newEnv(t *testing.T, cfg *Config, btime string, epoch int64, files ...string) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	if btime != "" {
		e.Fake.File("/proc/stat", "cpu 1 2 3 4\nbtime "+btime+"\n")
	}
	e.Fake.Expect("find /etc", strings.Join(files, "\n"))
	var stat []string
	for _, f := range files {
		stat = append(stat, fmt.Sprintf("%d 2026-08-25 14:00:00.123456789 +0000 %s", epoch, f))
	}
	e.Fake.Expect("stat -c %Y %y %n", strings.Join(stat, "\n"))
	return e
}

func TestFilesRewrittenAtBootAreInformational(t *testing.T) {
	e := newEnv(t, nil, "1700000000", 1_700_000_010, "/etc/hostname", "/etc/hosts", "/etc/timezone") // 10s after boot
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	row := checktest.Rows(s)["/etc/hostname"]
	if row.Status != check.Info || row.Detail != "rewritten at boot" || row.Value != "2026-08-25 14:00:00" {
		t.Fatalf("/etc/hostname row: %+v", row)
	}
}

func TestSecurityRelevantEtcChangeAlerts(t *testing.T) {
	e := newEnv(t, nil, "1700000000", 1_700_086_400, "/etc/shadow") // a day after boot
	s := e.Run(Check{})
	if got := s.AlertMsgs(); !slices.Equal(got, []string{"Security-relevant /etc change: /etc/shadow"}) {
		t.Fatalf("alerts: %q", got)
	}
	if got := checktest.Rows(s)["/etc/shadow"]; got.Status != check.Caution || got.Detail != "security-relevant file" {
		t.Fatalf("/etc/shadow row: %+v", got)
	}
}

func TestSensitiveFileRewrittenAtBootStillAlerts(t *testing.T) {
	e := newEnv(t, nil, "1700000000", 1_700_000_010, "/etc/fstab")
	if len(e.Run(Check{}).Alerts) != 1 {
		t.Fatal("a sensitive file is sensitive even at boot")
	}
}

func TestManySensitiveChangesProduceOneGroupedAlert(t *testing.T) {
	// It used to emit one alert line per file, burying the interesting ones.
	e := newEnv(t, nil, "", 1_700_086_400, "/etc/passwd", "/etc/shadow", "/etc/group", "/etc/sudoers", "/etc/fstab")
	got := e.Run(Check{}).AlertMsgs()
	if len(got) != 1 || !strings.HasSuffix(got[0], "(+2 more)") {
		t.Fatalf("alerts: %q", got)
	}
	// Sorted, so the first three named are in path order.
	if want := "Security-relevant /etc change: /etc/fstab, /etc/group, /etc/passwd (+2 more)"; got[0] != want {
		t.Fatalf("alert = %q, want %q", got[0], want)
	}
}

func TestOrdinaryEtcChurnIsInformational(t *testing.T) {
	e := newEnv(t, nil, "", 1_700_086_400, "/etc/apt/apt.conf.d/01autoremove")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if got := checktest.Rows(s)["Security-Relevant Changes"]; got.Status != check.OK || got.Value != "None" {
		t.Fatalf("Security-Relevant Changes row: %+v", got)
	}
	if got := checktest.Rows(s)["/etc/apt/apt.conf.d/01autoremove"]; got.Status != check.Info || got.Detail != "" {
		t.Fatalf("file row: %+v", got)
	}
}

func TestNoChangesIsOK(t *testing.T) {
	e := newEnv(t, nil, "", 0)
	s := e.Run(Check{})
	if got := checktest.Rows(s)["Changes"]; got.Status != check.OK || got.Value != "No files modified in the last 24 hours" {
		t.Fatalf("rows: %+v", s.Rows)
	}
	if e.Fake.Ran("stat") {
		t.Fatal("stat run with no files")
	}
}

func TestOneStatCallCoversEveryFile(t *testing.T) {
	e := newEnv(t, nil, "", 1_700_086_400, "/etc/b.conf", "/etc/a dir/x.conf", "/etc/c.conf")
	s := e.Run(Check{})
	var stats []string
	for _, c := range e.Fake.Calls() {
		if strings.HasPrefix(c, "stat ") {
			stats = append(stats, c)
		}
	}
	if len(stats) != 1 || !strings.HasSuffix(stats[0], "/etc/a dir/x.conf /etc/b.conf /etc/c.conf") {
		t.Fatalf("stat calls: %q", stats)
	}
	// A name with a space still gets its own mtime.
	if got := checktest.Rows(s)["/etc/a dir/x.conf"].Value; got != "2026-08-25 14:00:00" {
		t.Fatalf("mtime for a spaced name = %q", got)
	}
}

func TestTheListIsSortedAndCappedAtForty(t *testing.T) {
	var files []string
	for i := 50; i > 0; i-- {
		files = append(files, fmt.Sprintf("/etc/f%02d", i))
	}
	s := newEnv(t, nil, "", 1_700_086_400, files...).Run(Check{})
	labels := checktest.Labels(s)
	if len(labels) != 41 || labels[0] != "/etc/f01" || labels[39] != "/etc/f40" {
		t.Fatalf("labels: %q", labels)
	}
}

func TestCommVaultRegistryBackupsAreIgnoredByDefault(t *testing.T) {
	// Observed on a real server: a new .zst every 90 minutes.
	args := strings.Join(findArgs(Check{}.Defaults().(*Config)), " ")
	if !strings.Contains(args, "-not -path /etc/CommVaultRegistryBackups/*") {
		t.Fatalf("args: %s", args)
	}
}

func TestExtraIgnorePatternsComeFromTheConfig(t *testing.T) {
	args := strings.Join(findArgs(&Config{Toggle: check.On, Ignore: []string{"/etc/foo/*", "/etc/bar/*.bak"}}), " ")
	for _, want := range []string{"-not -path /etc/foo/*", "-not -path /etc/bar/*.bak", "-not -path */mtab"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
}

func TestIgnorePatternsAreCarriedVerbatim(t *testing.T) {
	// No shell is involved: a space or a semicolon inside a pattern must stay
	// inside that one argument.
	evil := "/etc/x y/*; rm -rf /"
	args := findArgs(&Config{Toggle: check.On, Ignore: []string{evil}})
	i := slices.Index(args, evil)
	if i < 2 || args[i-1] != "-path" || args[i-2] != "-not" {
		t.Fatalf("pattern not one argument after -not -path: %q", args)
	}
	if slices.Contains(args, "rm") || slices.Contains(args, ";") {
		t.Fatalf("pattern was split: %q", args)
	}
	if n := countOf(args, "-path"); n != 9 {
		t.Fatalf("%d -path exclusions, want 8 built-in + 1", n)
	}
}

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

func TestTheIgnoreListReachesTheFindCommand(t *testing.T) {
	e := newEnv(t, &Config{Toggle: check.On, Ignore: []string{"/etc/CommVaultRegistryBackups/*"}}, "", 0)
	e.Run(Check{})
	if !e.Fake.Ran("find /etc -maxdepth 3 -type f -mmin -1440") || !e.Fake.Ran("CommVaultRegistryBackups") {
		t.Fatalf("calls: %q", e.Fake.Calls())
	}
}
