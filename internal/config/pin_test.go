package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/thyarles/lhc/internal/config"
)

func assign(t *testing.T, kvs ...string) []config.Assignment {
	t.Helper()
	var out []config.Assignment
	for _, kv := range kvs {
		a, err := config.ParseAssignment(kv)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// active lists the dotted keys a file really sets.
func active(t *testing.T, src []byte) []string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(prefix string, n *yaml.Node)
	walk = func(prefix string, n *yaml.Node) {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := prefix+n.Content[i].Value, n.Content[i+1]
			if v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0 {
				walk(k+".", v)
				continue
			}
			out = append(out, k)
		}
	}
	if doc.Kind != 0 {
		walk("", doc.Content[0])
	}
	return out
}

func get(t *testing.T, src []byte, key string) string {
	t.Helper()
	c, err := config.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	flat, _ := c.Flatten()
	for _, s := range flat {
		if s.Key == key {
			return s.Value
		}
	}
	t.Fatalf("no %s", key)
	return ""
}

// The file a v0.6 install wrote: the whole example with that day's
// zombie defaults, and what install.sh's --set put in.
func oldFile(t *testing.T) []byte {
	t.Helper()
	src := strings.Replace(string(config.Example), "zombie_caution: 2500", "zombie_caution: 10", 1)
	src = strings.Replace(src, "zombie_unhealthy: 3000", "zombie_unhealthy: 50", 1)
	out, err := config.Set([]byte(src), assign(t,
		"smtp.host=mail.example.com", "email.daily_recipients=ops@example.com",
		"hub.collectors=http://collector.example.com:8461", "checks.disk.caution=85"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheCommentedExampleSetsNothing(t *testing.T) {
	ex := config.CommentedExample()
	if keys := active(t, ex); len(keys) != 0 {
		t.Fatalf("active keys: %v", keys)
	}
	if !bytes.Contains(ex, []byte("# lhc configuration")) || !bytes.Contains(ex, []byte("    #zombie_caution: 2500")) {
		t.Fatal("the explanations or the commented defaults are gone")
	}
	c, err := config.Parse(ex)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Marshal()
	b, _ := config.Default().Marshal()
	if !bytes.Equal(a, b) {
		t.Fatal("the commented example is not the defaults")
	}
}

func TestANewFileHasOnlyWhatWasSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SetFile(path, assign(t, "smtp.host=mail.example.com", "checks.processes.zombie_caution=100")); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(path)
	if keys := active(t, src); strings.Join(keys, " ") != "smtp.host checks.processes.zombie_caution" {
		t.Fatalf("active keys: %v\n%s", keys, src)
	}
	// Edited in place: same lines as the commented example, the sibling
	// still commented out under its now active section.
	if n, m := bytes.Count(src, []byte("\n")), bytes.Count(config.CommentedExample(), []byte("\n")); n != m {
		t.Fatalf("%d lines, the example has %d", n, m)
	}
	for _, want := range []string{"\nsmtp:\n  host: mail.example.com\n  #port: 25\n", "\n  processes:\n", "    zombie_caution: 100\n    #zombie_unhealthy: 3000\n"} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("no %q in:\n%s", want, src)
		}
	}
}

// Seen for real: hosts installed on v0.6/v0.7 kept zombie limits 10/50
// after the defaults moved to 2500/3000, and stayed UNHEALTHY at 140.
func TestUnpinLetsOldDefaultsFollowTheNewOnes(t *testing.T) {
	out, done, err := config.Unpin(oldFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if get(t, out, "checks.processes.zombie_caution") != "2500" || get(t, out, "checks.processes.zombie_unhealthy") != "3000" {
		t.Fatal("the old zombie defaults are still pinned")
	}
	for key, want := range map[string]string{
		"smtp.host": "mail.example.com", "email.daily_recipients": "[ops@example.com]",
		"hub.collectors": "[http://collector.example.com:8461]", "checks.disk.caution": "85",
	} {
		if got := get(t, out, key); got != want {
			t.Errorf("%s = %s, want %s: someone's choice must stay", key, got, want)
		}
	}
	if keys := active(t, out); strings.Join(keys, " ") != "smtp.host email.daily_recipients hub.collectors checks.disk.caution" {
		t.Fatalf("active keys: %v", keys)
	}
	// The commented line shows today's default, not the old one.
	if !bytes.Contains(out, []byte("    #zombie_caution: 2500\n")) || bytes.Contains(out, []byte("zombie_caution: 10")) {
		t.Fatal("the commented line shows the old default")
	}
	past := 0
	for _, u := range done {
		if u.Past {
			past++
			if u.Key == "checks.processes.zombie_caution" && (u.Value != "10" || u.Default != "2500") {
				t.Fatalf("%+v", u)
			}
		}
	}
	if past != 2 || len(done) < 50 {
		t.Fatalf("%d past, %d in all", past, len(done))
	}
	if !bytes.Contains(out, []byte("# lhc configuration")) {
		t.Fatal("comments lost")
	}

	again, done2, err := config.Unpin(out)
	if err != nil || !bytes.Equal(again, out) || len(done2) != 0 {
		t.Fatalf("not idempotent: %v, %d more", err, len(done2))
	}
}

func TestAValueThatWasNeverADefaultStays(t *testing.T) {
	src := []byte("checks:\n  processes:\n    zombie_caution: 100\n    zombie_unhealthy: 50\n")
	out, done, err := config.Unpin(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "checks:\n  processes:\n    zombie_caution: 100\n    #zombie_unhealthy: 3000\n" || len(done) != 1 {
		t.Fatalf("%d unpinned:\n%s", len(done), out)
	}
}

func TestAStaleCommentedDefaultIsRefreshed(t *testing.T) {
	src := []byte("checks:\n  processes:\n    zombie_caution: 100\n    #zombie_unhealthy: 50  # per host\n")
	out, done, err := config.Unpin(src)
	if err != nil || len(done) != 0 {
		t.Fatal(err, done)
	}
	if string(out) != "checks:\n  processes:\n    zombie_caution: 100\n    #zombie_unhealthy: 3000  # per host\n" {
		t.Fatalf("%s", out)
	}
}

func TestSetAddsAMissingKeyUnderItsSection(t *testing.T) {
	out, err := config.Set([]byte("smtp:\n  host: a.example.com\n\nemail:\n  html_mode: both\n"), assign(t, "smtp.port=26", "checks.disk.caution=85"))
	if err != nil {
		t.Fatal(err)
	}
	want := "smtp:\n  host: a.example.com\n  port: 26\n\nemail:\n  html_mode: both\nchecks:\n  disk:\n    caution: 85\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

// Shapes the line editor leaves to the node editor.
func TestSetStillHandlesFlowSections(t *testing.T) {
	for _, src := range []string{
		"checks:\n  cpu: { enabled: true, caution: 80 }\n",
		"checks: {}\n",
		"checks:\n",
	} {
		out, err := config.Set([]byte(src), assign(t, "checks.cpu.caution=70"))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if got := get(t, out, "checks.cpu.caution"); got != "70" {
			t.Fatalf("%q: %s\n%s", src, got, out)
		}
	}
}

func TestUnpinFileKeepsABackupAndLeavesACleanFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	old := oldFile(t)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if done, err := config.UnpinFile(path); err != nil || len(done) == 0 {
		t.Fatal(done, err)
	}
	if bak, _ := os.ReadFile(path + ".bak"); !bytes.Equal(bak, old) {
		t.Fatal("no backup of the old file")
	}
	if err := os.Remove(path + ".bak"); err != nil {
		t.Fatal(err)
	}
	if done, err := config.UnpinFile(path); err != nil || len(done) != 0 {
		t.Fatal(done, err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("a backup although nothing changed")
	}
	if done, err := config.UnpinFile(filepath.Join(t.TempDir(), "none.yaml")); err != nil || done != nil {
		t.Fatal(done, err)
	}
}
