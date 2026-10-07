// Package cli is the lhc command line. Each subcommand lives in its own
// file; this one holds what they share: loading the config, resolving paths
// and identifying the host.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/config"
	"github.com/thyarles/lhc-go/internal/host"
	"github.com/thyarles/lhc-go/internal/paths"
	"github.com/thyarles/lhc-go/internal/runner"
)

// app is the state shared by the subcommands, filled lazily.
type app struct {
	configFlag string
	verbose    bool
	stdout     io.Writer
	stderr     io.Writer

	pathEnv  paths.Env
	cfgPath  string
	cfg      *config.Config
	cfgFound bool
	paths    paths.Paths
	runner   *runner.System
	now      func() time.Time
}

func newApp(stdout, stderr io.Writer) *app {
	return &app{stdout: stdout, stderr: stderr, pathEnv: paths.Current(), now: time.Now}
}

// configPath is the file this invocation reads and writes.
func (a *app) configPath() string {
	if a.cfgPath == "" {
		p := paths.ConfigFile(a.configFlag, a.pathEnv)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		a.cfgPath = p
	}
	return a.cfgPath
}

// load reads the config and resolves the data paths. A missing file is not
// an error: the defaults apply.
func (a *app) load() error {
	cfg, found, err := config.Load(a.configPath())
	if err != nil {
		return err
	}
	a.cfg, a.cfgFound = cfg, found
	a.paths = paths.Data(a.pathEnv, cfg.Paths.StateDir, cfg.Paths.ReportDir, cfg.Paths.LogFile)
	a.paths.Config = a.configPath()
	if a.runner == nil {
		a.runner = runner.New()
	}
	return nil
}

func (a *app) logger(w io.Writer) *slog.Logger {
	level := slog.LevelWarn
	if a.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// hostInfo identifies the machine once per run.
func (a *app) hostInfo() check.Host {
	kernel := host.Kernel()
	resolve := host.Cached(func() string { return host.Resolve(kernel) })
	h := check.Host{
		Kernel:     kernel,
		Label:      host.Label(a.cfg.Hostname, kernel, resolve),
		Resolved:   resolve(),
		PkgManager: host.PkgManager(a.runner.LookPath),
		OS:         map[string]string{},
	}
	if b, err := a.runner.ReadFile("/etc/os-release"); err == nil {
		h.OS = host.ParseOSRelease(b)
	}
	return h
}

// fromAddress is smtp.from, or lhc@<this host> — distinct per host, which is
// the point, and qualified so strict relays accept it.
func (a *app) fromAddress(h check.Host) string {
	if a.cfg.SMTP.From != "" {
		return a.cfg.SMTP.From
	}
	return "lhc@" + host.MailDomain(h.Label, func() string { return h.Resolved })
}

func (a *app) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.stdout, format+"\n", args...)
}

func isRoot() bool { return os.Geteuid() == 0 }
