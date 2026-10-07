package docker

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

const psCmd = "docker ps -a"

func newEnv(t *testing.T, cfg *Config) *checktest.Env {
	var c check.Config
	if cfg != nil {
		c = cfg
	}
	e := checktest.NewEnv(t, Check{}, c)
	e.Fake.Tool("docker")
	return e
}

func TestDockerStatusClassification(t *testing.T) {
	cases := []struct {
		status string
		want   check.Status
	}{
		{"Up 3 days", check.OK},
		{"Up 3 days (healthy)", check.OK},
		{"Up 2 hours (unhealthy)", check.Caution},       // docker's own healthcheck
		{"Restarting (1) 5 seconds ago", check.Caution}, // crash loop
		{"Exited (1) 24 seconds ago", check.Caution},    // fresh crash
		{"Exited (255) 6 weeks ago", check.Info},        // long dead, deliberate
		{"Exited (0) About an hour ago", check.Info},    // clean one-shot / k8s init
		{"Exited (0) 6 weeks ago", check.Info},
		{"Created", check.Info},
	}
	for _, c := range cases {
		e := newEnv(t, nil)
		e.Fake.Expect(psCmd, "c1\t"+c.status+"\timage:latest")
		row, ok := checktest.Rows(e.Run(Check{}))["c1"]
		if !ok || row.Status != c.want {
			t.Errorf("%q: %+v, want %v", c.status, row, c.want)
		}
		if ok && row.Value != c.status+"  [image:latest]" {
			t.Errorf("%q: value %q", c.status, row.Value)
		}
	}
}

func TestDockerOldStoppedContainersRaiseNoAlert(t *testing.T) {
	// The exact machine state that produced "19 stopped containers" daily.
	e := newEnv(t, nil)
	e.Fake.Expect(psCmd, strings.Join([]string{
		"eleicoes-web-1\tExited (255) 6 weeks ago\tweb:latest",
		"eleicoes-api-1\tExited (255) 6 weeks ago\tapi:latest",
		"k8s_POD_helm-install\tExited (0) About an hour ago\tpause:3.6",
		"monitora87-db\tExited (0) 6 weeks ago\tmysql:8.0",
	}, "\n"))
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if s.Status == check.Caution {
		t.Fatal("section is CAUTION")
	}
	if got := checktest.Rows(s)["Stopped/Exited Containers"].Value; got != "4" {
		t.Fatalf("stopped = %q", got)
	}
}

func TestDockerFreshCrashStillAlerts(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect(psCmd, "old-stack\tExited (255) 6 weeks ago\tweb:latest\nkutt-server-1\tExited (1) 24 seconds ago\tkutt-server")
	got := e.Run(Check{}).AlertMsgs()
	if !slices.Equal(got, []string{"Docker: kutt-server-1 (crashed within the last 24h)"}) {
		t.Fatalf("alerts: %q", got)
	}
}

func TestDockerRecentWindowIsConfigurable(t *testing.T) {
	e := newEnv(t, &Config{Toggle: check.On, RecentHours: 1})
	e.Fake.Expect(psCmd, "c1\tExited (1) 3 hours ago\timage")
	checktest.NoAlerts(t, e.Run(Check{}))
}

func TestStatusAgeParsing(t *testing.T) {
	cases := []struct {
		status string
		hours  float64
	}{
		{"Exited (1) 30 seconds ago", 30.0 / 3600},
		{"Exited (1) 5 minutes ago", 5.0 / 60},
		{"Exited (1) About an hour ago", 1},
		{"Exited (1) 3 days ago", 72},
		{"Exited (1) 6 weeks ago", 6 * 168},
		// "a second"/"an hour" have no digit; the article reads as 1.
		{"Exited (1) Less than a second ago", 1.0 / 3600},
	}
	for _, c := range cases {
		if got := statusAgeHours(c.status); math.Abs(got-c.hours) > c.hours*0.01 {
			t.Errorf("statusAgeHours(%q) = %v, want %v", c.status, got, c.hours)
		}
	}
}

func TestStatusAgeOfUnparseableStatusIsTreatedAsAncient(t *testing.T) {
	if got := statusAgeHours("Created"); got <= 1e6 {
		t.Fatalf("statusAgeHours(Created) = %v", got)
	}
}

func TestUninstalledDockerDoesNotGetItsOwnSection(t *testing.T) {
	// Docker absent used to cost a whole panel.
	s := checktest.NewEnv(t, Check{}, nil).Run(Check{})
	if s.Applicable {
		t.Fatal("section applicable without docker")
	}
	if len(s.MissingTools) != 1 || !s.MissingTools[0].Optional ||
		s.MissingTools[0].Package("dnf") != "docker-ce" || s.MissingTools[0].Package("apt-get") != "docker.io" ||
		s.MissingTools[0].Package("zypper") != "docker" {
		t.Fatalf("missing tools: %+v", s.MissingTools)
	}
}

func TestStoppedDaemonCollapsesToOneLine(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect(psCmd, "").Code(1).Stderr("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	s := e.Run(Check{})
	checktest.NoAlerts(t, s)
	if s.Applicable || len(s.Rows) != 1 || s.Rows[0].Value == "None" {
		t.Fatalf("section: %+v", s)
	}
}

func TestOtherListingErrorsAreNotReportedAsNone(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect(psCmd, "").Code(1).Stderr("permission denied while trying to connect to the Docker daemon socket")
	row := checktest.Rows(e.Run(Check{}))["Containers"]
	if row.Value != "Could not be listed" || !strings.Contains(row.Detail, "permission denied") {
		t.Fatalf("Containers row: %+v", row)
	}
}

func TestNoContainers(t *testing.T) {
	s := newEnv(t, nil).Run(Check{})
	if got := checktest.Rows(s)["Containers"]; got.Value != "None" || got.Status != check.Info {
		t.Fatalf("Containers row: %+v", got)
	}
}

func TestHealthyContainersAreACountNotAWall(t *testing.T) {
	e := newEnv(t, nil)
	var lines []string
	for i := range 15 {
		lines = append(lines, fmt.Sprintf("pod-%02d\tUp 2 days\tpause:3.6", i))
	}
	e.Fake.Expect(psCmd, strings.Join(lines, "\n"))
	s := e.Run(Check{})
	rows := checktest.Rows(s)
	if rows["Running"].Value != "15 container(s) up" || rows["…"].Value != "and 5 more container(s)" {
		t.Fatalf("rows: %q", checktest.Labels(s))
	}
}

func TestManyProblemsAreOneAlert(t *testing.T) {
	e := newEnv(t, nil)
	var lines []string
	for i := range 5 {
		lines = append(lines, fmt.Sprintf("bad-%d\tRestarting (1) 5 seconds ago\timg", i))
	}
	e.Fake.Expect(psCmd, strings.Join(lines, "\n"))
	got := e.Run(Check{}).AlertMsgs()
	if len(got) != 1 || !strings.HasSuffix(got[0], "bad-2 (restart loop) (+2 more)") {
		t.Fatalf("alerts: %q", got)
	}
}

func TestDiskUsageRows(t *testing.T) {
	e := newEnv(t, nil)
	e.Fake.Expect(psCmd, "c1\tUp 3 days\timg")
	e.Fake.Expect("docker system df", "Images\t5\t2\t1.2GB\t800MB (66%)\nLocal Volumes\t3\t1\t40MB\t10MB (25%)")
	s := e.Run(Check{})
	rows := checktest.Rows(s)
	if got := rows["Local Volumes"].Value; got != "3 total, 1 active, 40MB (reclaimable 10MB (25%))" {
		t.Fatalf("Local Volumes = %q", got)
	}
	if !slices.Contains(checktest.Labels(s), "Docker Disk Usage") {
		t.Fatal("no separator")
	}
}

func TestValidate(t *testing.T) {
	c := Check{}.Defaults().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.RecentHours = 0
	if c.Validate() == nil {
		t.Fatal("zero recent_hours accepted")
	}
}
