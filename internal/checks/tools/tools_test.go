package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

func TestMissingToolsAreInformationalNotCaution(t *testing.T) {
	// A tool missing yesterday is missing today for the same reason.
	s := checktest.NewEnv(t, Check{}, nil).Run(Check{})
	if s.Status != check.Info {
		t.Fatalf("status %v", s.Status)
	}
	checktest.NoAlerts(t, s)
}

func TestOptionalToolsThatAreAbsentSayNothing(t *testing.T) {
	// A plain web server has no docker and never will. Suggesting it install
	// docker-ce (or kubectl) every morning can only ever be permanent noise.
	labels := strings.Join(checktest.Labels(checktest.NewEnv(t, Check{}, nil).Run(Check{})), " ")
	for _, absent := range []string{"docker", "kubectl", "crictl", "rkhunter", "fail2ban-client"} {
		if strings.Contains(labels, absent) {
			t.Errorf("%s listed although optional and absent: %s", absent, labels)
		}
	}
}

func TestRequiredToolsThatAreAbsentStillOfferTheInstallCommand(t *testing.T) {
	// These reduce coverage of checks the host IS running, so they stay.
	e := checktest.NewEnv(t, Check{}, nil)
	e.Host.PkgManager = "dnf"
	s := e.Run(Check{})
	if got := checktest.Labels(s); !slices.Equal(got, []string{"mpstat", "ss"}) {
		t.Fatalf("labels %v", got)
	}
	rows := checktest.Rows(s)
	if v := rows["mpstat"].Value; v != "Not installed — run: dnf install -y sysstat" {
		t.Fatalf("mpstat %q", v)
	}
	if rows["mpstat"].Detail != "Reduces coverage of some checks" {
		t.Fatalf("detail %q", rows["mpstat"].Detail)
	}
	if len(s.MissingTools) != 2 || s.MissingTools[0].Name != "mpstat" || s.MissingTools[1].Name != "ss" {
		t.Fatalf("missing tools %+v", s.MissingTools)
	}
}

func TestAnOptionalToolAppearsOnceItIsInstalled(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	e.Fake.Tool("docker")
	row, ok := checktest.Rows(e.Run(Check{}))["[optional] docker"]
	if !ok || row.Status != check.OK || row.Value != "Installed" {
		t.Fatalf("row %+v", row)
	}
}

func TestEverythingInstalledIsOK(t *testing.T) {
	e := checktest.NewEnv(t, Check{}, nil)
	for _, tl := range Tools {
		e.Fake.Tool(tl.Name)
	}
	s := e.Run(Check{})
	if s.Status != check.OK || len(s.Rows) != len(Tools) || len(s.MissingTools) != 0 {
		t.Fatalf("section %+v", s)
	}
}

func TestInstallCommandPerPackageManager(t *testing.T) {
	cases := map[string]string{
		"dnf":     "Not installed — run: dnf install -y iproute",
		"yum":     "Not installed — run: yum install -y iproute",
		"apt-get": "Not installed — run: apt-get install -y iproute2",
		"zypper":  "Not installed — run: zypper --non-interactive install iproute2",
		"":        "Not installed — run: yum install -y iproute  OR  apt-get install -y iproute2  OR  zypper install iproute2",
	}
	for pm, want := range cases {
		e := checktest.NewEnv(t, Check{}, nil)
		e.Fake.Tool("mpstat")
		e.Host.PkgManager = pm
		if got := checktest.Rows(e.Run(Check{}))["ss"].Value; got != want {
			t.Errorf("%q: %q, want %q", pm, got, want)
		}
	}
}

func TestToolList(t *testing.T) {
	// lhc speaks SMTP itself; nothing should ask a host to install an MTA.
	seen := map[string]bool{}
	for _, tl := range Tools {
		if tl.Name == "postfix" {
			t.Error("postfix is back in the tool list")
		}
		if seen[tl.Name] {
			t.Errorf("%s listed twice", tl.Name)
		}
		seen[tl.Name] = true
		for _, pm := range []string{"dnf", "apt-get", "zypper"} {
			if tl.Package(pm) == "" {
				t.Errorf("%s has no %s package", tl.Name, pm)
			}
		}
	}
	want := map[string]string{
		"mpstat": "sysstat", "ss": "iproute2", "fail2ban-client": "fail2ban", "rkhunter": "rkhunter",
		"docker": "docker", "kubectl": "kubernetes-client", "crictl": "cri-tools",
	}
	for name, pkg := range want {
		if !seen[name] {
			t.Errorf("%s missing", name)
		}
		for _, tl := range Tools {
			if tl.Name == name && tl.Package("zypper") != pkg {
				t.Errorf("%s on SUSE = %q, want %q", name, tl.Package("zypper"), pkg)
			}
		}
	}
}
