package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/thyarles/lhc/internal/checks/all"
	"github.com/thyarles/lhc/internal/config"
)

func TestTheExampleIsExactlyTheDefaults(t *testing.T) {
	// The example is the documentation. If it drifts from the code, a host
	// reading it gets values nobody intended.
	got, err := config.Parse(config.Example)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	a, _ := got.Marshal()
	b, _ := config.Default().Marshal()
	if string(a) != string(b) {
		t.Fatalf("example differs from defaults:\n--- example\n%s\n--- defaults\n%s", a, b)
	}
}

func TestEveryRegisteredCheckIsDocumentedInTheExample(t *testing.T) {
	for _, name := range config.Known() {
		if !strings.Contains(string(config.Example), "\n  "+name+":\n") {
			t.Errorf("checks.%s has no block in config.example.yaml", name)
		}
	}
}

func TestAnEmptyFileMeansDefaults(t *testing.T) {
	c, err := config.Parse(nil)
	if err != nil || !reflect.DeepEqual(c.Email, config.Default().Email) {
		t.Fatal(c, err)
	}
}

func TestOnlyWhatTheFileSaysChanges(t *testing.T) {
	c, err := config.Parse([]byte("email:\n  daily_recipients: [ops@example.com]\nchecks:\n  disk:\n    caution: 85\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Email.HTMLMode != "inline" || len(c.Email.DailyRecipients) != 1 || c.SMTP.Port != 25 {
		t.Fatalf("%+v", c.Email)
	}
	flat, _ := c.Diff()
	keys := map[string]string{}
	for _, s := range flat {
		keys[s.Key] = s.Value
	}
	if keys["checks.disk.caution"] != "85" || keys["email.daily_recipients"] != "[ops@example.com]" || len(keys) != 2 {
		t.Fatalf("diff %v", keys)
	}
}

func TestUnknownKeysFailLoudly(t *testing.T) {
	for _, bad := range []string{
		"smtp:\n  hots: x\n",
		"checks:\n  disk:\n    cuation: 85\n",
		"checks:\n  dsik:\n    enabled: false\n",
		"bogus: 1\n",
	} {
		if _, err := config.Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{
		"smtp:\n  tls: maybe\n",
		"smtp:\n  username: u\n",
		"email:\n  html_mode: pdf\n",
		"email:\n  daily_recipients: [not-an-address]\n",
		"alerts:\n  notify_all_on: info\n",
		"schedule:\n  time: \"25:00\"\n",
		"schedule:\n  every: 90m\n",
		"schedule:\n  random_window: soon\n",
		"checks:\n  cpu:\n    caution: 99\n",
	} {
		c, err := config.Parse([]byte(bad))
		if err == nil {
			err = c.Validate()
		}
		if err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDurationsInTheConfig(t *testing.T) {
	c, err := config.Parse([]byte("alerts:\n  remind_caution: 7d\n  remind_unhealthy: 12\n  forget_after: 2h30m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerts.RemindCaution.D() != 168*time.Hour || c.Alerts.RemindUnhealthy.D() != 12*time.Hour || c.Alerts.ForgetAfter.D() != 150*time.Minute {
		t.Fatalf("%+v", c.Alerts)
	}
}

func TestSetKeepsCommentsAndValidates(t *testing.T) {
	src := []byte("# my host\nsmtp:\n  host: old.example.com # the relay\n")
	assigns := []config.Assignment{}
	for _, kv := range []string{"smtp.host=mail.example.com", "schedule.time=06:30", "email.daily_recipients=a@x.org, b@x.org", "checks.disk.caution=85"} {
		a, err := config.ParseAssignment(kv)
		if err != nil {
			t.Fatal(err)
		}
		assigns = append(assigns, a)
	}
	out, err := config.Set(src, assigns)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# my host", "# the relay", "mail.example.com"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q missing from:\n%s", want, s)
		}
	}
	c, err := config.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.Time != "06:30" || len(c.Email.DailyRecipients) != 2 {
		t.Fatalf("%+v %+v", c.Schedule, c.Email)
	}
}

func TestSetRejectsTyposAndBadValues(t *testing.T) {
	for _, kv := range []string{"smtp.hots=x", "schedule.time=25:99", "checks.nope.enabled=false", "smtp=x", "smtp.port=many"} {
		a, err := config.ParseAssignment(kv)
		if err == nil {
			_, err = config.Set([]byte(""), []config.Assignment{a})
		}
		if err == nil {
			t.Errorf("accepted %q", kv)
		}
	}
}

func TestSetFileCreatesFromTheExampleAndKeepsABackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lhc", "config.yaml")
	a, _ := config.ParseAssignment("smtp.host=one.example.com")
	if err := config.SetFile(path, []config.Assignment{a}); err != nil {
		t.Fatal(err)
	}
	b, _ := config.ParseAssignment("smtp.host=two.example.com")
	if err := config.SetFile(path, []config.Assignment{b}); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(path)
	bak, _ := os.ReadFile(path + ".bak")
	if !strings.Contains(string(cur), "two.example.com") || !strings.Contains(string(bak), "one.example.com") {
		t.Fatal("backup or update missing")
	}
	if !strings.Contains(string(cur), "# lhc configuration") {
		t.Fatal("example comments lost")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: the file holds the SMTP password", fi.Mode().Perm())
	}
}

func TestPasswordIsMaskedInShow(t *testing.T) {
	c, err := config.Parse([]byte("smtp:\n  tls: starttls\n  username: u\n  password: hunter2\n"))
	if err != nil {
		t.Fatal(err)
	}
	flat, _ := c.Flatten()
	for _, s := range flat {
		if strings.Contains(s.Value, "hunter2") {
			t.Fatal("password shown")
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{"168h": 168 * time.Hour, "7d": 168 * time.Hour, "90m": 90 * time.Minute, "4": 4 * time.Hour, "0": 0}
	for in, want := range cases {
		if got, err := config.ParseDuration(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, d := range []time.Duration{0, 90 * time.Minute, 168 * time.Hour, 45 * time.Second} {
		back, err := config.ParseDuration(config.Duration(d).String())
		if err != nil || back != d {
			t.Errorf("round trip %v: %v %v", d, back, err)
		}
	}
}
