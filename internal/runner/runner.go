// Package runner is the only door from lhc to the operating system's
// commands and files. Checks receive a Runner and never touch os/exec or the
// filesystem directly (the linter enforces it), which is what lets every
// check be tested with checktest.Runner instead of a real host.
package runner

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DefaultTimeout applies to a command whose context carries no deadline.
const DefaultTimeout = 30 * time.Second

// Result of one command. Code is -1 when the command could not be started or
// was killed by its timeout; Err says why.
type Result struct {
	Code   int
	Stdout string
	Stderr string
	Err    error
}

// OK reports a zero exit status.
func (r Result) OK() bool { return r.Code == 0 && r.Err == nil }

// Runner runs commands and reads files on behalf of the checks.
type Runner interface {
	// Run executes name with args directly, no shell. Output is trimmed.
	Run(ctx context.Context, name string, args ...string) Result
	// Shell runs script with sh -c. Only for real pipelines; anything that
	// is a single command belongs in Run.
	Shell(ctx context.Context, script string) Result
	// LookPath finds an executable on the runner's PATH, or checks that an
	// absolute path is executable.
	LookPath(name string) (string, bool)
	// Readable reports whether path exists and can be opened for reading.
	Readable(path string) bool
	// Exists reports whether anything exists at path (no symlink follow).
	Exists(path string) bool
	// IsSocket reports whether path is a unix socket.
	IsSocket(path string) bool
	// ReadFile returns the file's contents.
	ReadFile(path string) ([]byte, error)
	// ReadDir lists the entries of a directory, sorted by name.
	ReadDir(path string) ([]fs.DirEntry, error)
}

// Extra directories searched after the inherited PATH. Cron and systemd hand
// a scheduled run PATH=/usr/bin:/bin, while the tools a check needs often
// live in sbin or in a distribution-specific place: rke2 exports
// /var/lib/rancher/rke2/bin only from an interactive profile, which is
// precisely what a scheduled run never reads.
var extraPath = []string{
	"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
	"/var/lib/rancher/rke2/bin", "/opt/bin", "/snap/bin",
}

// System is the real Runner.
type System struct {
	path []string
	env  []string
}

// New builds the real Runner. Children get LC_ALL=C so that df, ss,
// systemctl and dnf print the same thing on every locale, a full PATH, and
// HOME=/root when systemd started us without one.
//
// LHC_PATH_OVERRIDE, when set, is searched before everything else. It exists
// for the end-to-end tests, which put fake commands there.
func New() *System {
	var dirs []string
	if o := os.Getenv("LHC_PATH_OVERRIDE"); o != "" {
		dirs = append(dirs, filepath.SplitList(o)...)
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	dirs = append(dirs, extraPath...)
	dirs = dedupe(dirs)

	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "LC_ALL", "LANG", "LANGUAGE":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PATH="+strings.Join(dirs, ":"), "LC_ALL=C", "LANG=C")
	if os.Getenv("HOME") == "" {
		env = append(env, "HOME=/root")
	}
	return &System{path: dirs, env: env}
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, d := range in {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Path is the search path children see, for `lhc paths` and diagnostics.
func (s *System) Path() []string { return s.path }

func (s *System) Run(ctx context.Context, name string, args ...string) Result {
	bin, ok := s.LookPath(name)
	if !ok {
		return Result{Code: -1, Err: &exec.Error{Name: name, Err: exec.ErrNotFound}}
	}
	return s.exec(ctx, bin, args...)
}

func (s *System) Shell(ctx context.Context, script string) Result {
	return s.exec(ctx, "/bin/sh", "-c", script)
}

func (s *System) exec(ctx context.Context, bin string, args ...string) Result {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = s.env
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// CommandContext kills only the direct child. A pipeline under sh -c
	// would leave kubectl or find running after the timeout, still holding
	// the output pipes open, so put the child in its own process group and
	// kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	res := Result{
		Stdout: strings.TrimSpace(stdout.String()),
		Stderr: strings.TrimSpace(stderr.String()),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.Code, res.Err = -1, ctx.Err()
		if res.Stderr == "" {
			res.Stderr = "timeout"
		}
	case errors.As(err, &exitErr):
		res.Code = exitErr.ExitCode()
	default:
		res.Code, res.Err = -1, err
	}
	return res
}

func (s *System) LookPath(name string) (string, bool) {
	if strings.Contains(name, "/") {
		return name, isExecutable(name)
	}
	for _, dir := range s.path {
		p := filepath.Join(dir, name)
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0
}

func (s *System) Readable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func (s *System) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (s *System) IsSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Type() == fs.ModeSocket
}

func (s *System) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (s *System) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
