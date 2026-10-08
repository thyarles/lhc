// Package checktest provides hermetic fakes for testing checks: a scripted
// Runner, an in-memory state store and an Env builder. No root, no network,
// and no dependence on what happens to be installed on the machine running
// the tests.
package checktest

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/runner"
	"github.com/thyarles/lhc/internal/state"
)

// Rule routes commands containing a substring to a canned result.
type Rule struct {
	substr string
	res    runner.Result
	once   bool
	used   bool
}

// Code sets the exit status.
func (r *Rule) Code(n int) *Rule { r.res.Code = n; return r }

// Stderr sets the standard error.
func (r *Rule) Stderr(s string) *Rule { r.res.Stderr = s; return r }

// Once consumes the rule after its first match, so repeated calls to the
// same command can return different output.
func (r *Rule) Once() *Rule { r.once = true; return r }

// Runner is a fake runner.Runner. Commands are matched by substring of the
// full command line, first rule wins; anything unmatched returns empty
// success, which mirrors how the checks treat a command with no output.
type Runner struct {
	mu      sync.Mutex
	rules   []*Rule
	calls   []string
	tools   map[string]bool
	files   map[string][]byte
	dirs    map[string]bool
	sockets map[string]bool
	unread  map[string]bool
}

// NewRunner returns an empty fake: no tools, no files.
func NewRunner() *Runner {
	return &Runner{
		tools: map[string]bool{}, files: map[string][]byte{}, dirs: map[string]bool{},
		sockets: map[string]bool{}, unread: map[string]bool{},
	}
}

// Expect adds a rule. Stdout is trimmed like the real runner's.
func (f *Runner) Expect(substr, stdout string) *Rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &Rule{substr: substr, res: runner.Result{Stdout: strings.TrimSpace(stdout)}}
	f.rules = append(f.rules, r)
	return r
}

// Tool makes LookPath find a command. A bare name is found at
// /usr/bin/<name>; an absolute path is found exactly there.
func (f *Runner) Tool(names ...string) *Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		f.tools[n] = true
	}
	return f
}

// File creates a readable file.
func (f *Runner) File(p, content string) *Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = []byte(content)
	for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
		f.dirs[d] = true
	}
	return f
}

// Unreadable creates a file that exists but cannot be read.
func (f *Runner) Unreadable(p string) *Runner {
	f.File(p, "")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unread[p] = true
	return f
}

// Dir creates an empty directory.
func (f *Runner) Dir(p string) *Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
	return f
}

// Socket creates a unix socket.
func (f *Runner) Socket(p string) *Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sockets[p] = true
	return f
}

func (f *Runner) match(cmd string) runner.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	for _, r := range f.rules {
		if r.used || !strings.Contains(cmd, r.substr) {
			continue
		}
		if r.once {
			r.used = true
		}
		return r.res
	}
	return runner.Result{}
}

func (f *Runner) Run(_ context.Context, name string, args ...string) runner.Result {
	return f.match(strings.Join(append([]string{name}, args...), " "))
}

func (f *Runner) Shell(_ context.Context, script string) runner.Result { return f.match(script) }

func (f *Runner) LookPath(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.tools[name] {
		return "", false
	}
	if strings.HasPrefix(name, "/") {
		return name, true
	}
	return "/usr/bin/" + name, true
}

func (f *Runner) Readable(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[p]
	return ok && !f.unread[p]
}

func (f *Runner) Exists(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, file := f.files[p]
	return file || f.dirs[p] || f.sockets[p]
}

func (f *Runner) IsSocket(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sockets[p]
}

func (f *Runner) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	if f.unread[p] {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
	}
	return b, nil
}

func (f *Runner) ReadDir(p string) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[p] {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	seen := map[string]bool{}
	var out []fs.DirEntry
	add := func(child string, dir bool) {
		rel := strings.TrimPrefix(child, strings.TrimSuffix(p, "/")+"/")
		if rel == child || rel == "" {
			return
		}
		name, rest, nested := strings.Cut(rel, "/")
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, dirEntry{name: name, dir: dir || nested && rest != ""})
	}
	for fp := range f.files {
		add(fp, false)
	}
	for d := range f.dirs {
		add(d, true)
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// Calls returns every command line run so far.
func (f *Runner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Ran reports whether any command line contained substr.
func (f *Runner) Ran(substr string) bool {
	for _, c := range f.Calls() {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

type dirEntry struct {
	name string
	dir  bool
}

func (d dirEntry) Name() string { return d.name }
func (d dirEntry) IsDir() bool  { return d.dir }
func (d dirEntry) Type() fs.FileMode {
	if d.dir {
		return fs.ModeDir
	}
	return 0
}
func (d dirEntry) Info() (fs.FileInfo, error) { return nil, fmt.Errorf("checktest: no FileInfo") }

// Now is the fixed clock every Env starts with: Monday 2026-01-05 07:00 UTC.
var Now = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)

// Env is a check.Env wired to fakes. Change any field before running.
type Env struct {
	*check.Env
	Fake  *Runner
	Store *state.Memory
}

// NewEnv builds an Env for a check with the given config (nil = defaults).
func NewEnv(t testing.TB, c check.Check, cfg check.Config) *Env {
	t.Helper()
	if cfg == nil {
		cfg = c.Defaults()
	}
	fake, store := NewRunner(), state.NewMemory()
	level := slog.LevelWarn
	if testing.Verbose() {
		level = slog.LevelDebug
	}
	return &Env{
		Env: &check.Env{
			Runner: fake,
			State:  store,
			Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
			Now:    func() time.Time { return Now },
			Host:   check.Host{Label: "testhost", Kernel: "testhost", Resolved: "testhost", OS: map[string]string{}},
			Config: cfg,
		},
		Fake:  fake,
		Store: store,
	}
}

// Seed stores a value as if a previous run had saved it.
func (e *Env) Seed(t testing.TB, key string, v any) {
	t.Helper()
	if err := e.Store.Save(key, v); err != nil {
		t.Fatal(err)
	}
}

// Loaded reads back what the run saved.
func (e *Env) Loaded(t testing.TB, key string, out any) bool {
	t.Helper()
	return e.Store.Load(key, out)
}

// Run executes the check against this Env.
func (e *Env) Run(c check.Check) *check.Section {
	return c.Run(context.Background(), e.Env)
}

// Rows indexes a section's rows by label; the last row with a label wins.
func Rows(s *check.Section) map[string]check.Row {
	out := map[string]check.Row{}
	for _, r := range s.Rows {
		out[r.Label] = r
	}
	return out
}

// Labels lists the row labels in order.
func Labels(s *check.Section) []string {
	out := make([]string, 0, len(s.Rows))
	for _, r := range s.Rows {
		out = append(out, r.Label)
	}
	return out
}

// Flagged returns the rows at CAUTION or worse.
func Flagged(s *check.Section) []check.Row {
	var out []check.Row
	for _, r := range s.Rows {
		if !r.Separator && r.Status.Flagged() {
			out = append(out, r)
		}
	}
	return out
}

// HasAlertContaining reports whether any alert message contains substr.
func HasAlertContaining(s *check.Section, substr string) bool {
	for _, a := range s.Alerts {
		if strings.Contains(a.Msg, substr) {
			return true
		}
	}
	return false
}

// NoAlerts fails the test when the section raised any alert. Most check
// tests are "must NOT alert" regressions; this is their assertion.
func NoAlerts(t testing.TB, s *check.Section) {
	t.Helper()
	if len(s.Alerts) > 0 {
		t.Fatalf("expected no alerts, got %q", s.AlertMsgs())
	}
}
