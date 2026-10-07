// Package paths decides where lhc keeps its config, state, reports and log,
// for root (a system install) and for an ordinary user (trying it out).
package paths

import (
	"os"
	"path/filepath"
)

// Paths are the resolved locations.
type Paths struct {
	Config  string
	State   string
	Reports string
	Log     string
}

// Env is what resolution depends on, so tests need not be root.
type Env struct {
	Root       bool
	Home       string
	XDGConfig  string
	XDGState   string
	LHCConfig  string // $LHC_CONFIG
	Executable string // os.Executable()
	Exists     func(string) bool
}

// Current reads the real environment.
func Current() Env {
	home, _ := os.UserHomeDir()
	exe, _ := os.Executable()
	return Env{
		Root:       os.Geteuid() == 0,
		Home:       home,
		XDGConfig:  os.Getenv("XDG_CONFIG_HOME"),
		XDGState:   os.Getenv("XDG_STATE_HOME"),
		LHCConfig:  os.Getenv("LHC_CONFIG"),
		Executable: exe,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
	}
}

// SystemConfig is where a root install keeps its config.
const SystemConfig = "/etc/lhc/config.yaml"

// ConfigFile picks the config file:
//
//	--config > $LHC_CONFIG > /etc/lhc/config.yaml (root)
//	         > ~/.config/lhc/config.yaml > config.yaml next to the binary
//	         > /etc/lhc/config.yaml if it exists (a user previewing a system install)
//
// For a non-root user with no file anywhere the answer is the per-user path,
// which is where `lhc config init` will create one.
func ConfigFile(flag string, e Env) string {
	if flag != "" {
		return flag
	}
	if e.LHCConfig != "" {
		return e.LHCConfig
	}
	if e.Root {
		return SystemConfig
	}
	user := filepath.Join(xdg(e.XDGConfig, e.Home, ".config"), "lhc", "config.yaml")
	candidates := []string{user}
	if e.Executable != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(e.Executable), "config.yaml"))
	}
	candidates = append(candidates, SystemConfig)
	for _, c := range candidates {
		if e.Exists(c) {
			return c
		}
	}
	return user
}

// Data returns the state, report and log locations. Non-empty overrides (from
// the config's paths: block) win.
func Data(e Env, stateDir, reportDir, logFile string) Paths {
	p := Paths{State: "/var/lib/lhc", Log: "/var/log/lhc.log"}
	if !e.Root {
		p.State = filepath.Join(xdg(e.XDGState, e.Home, filepath.Join(".local", "state")), "lhc")
		p.Log = ""
	}
	if stateDir != "" {
		p.State = stateDir
	}
	p.Reports = filepath.Join(p.State, "reports")
	if p.Log == "" {
		p.Log = filepath.Join(p.State, "lhc.log")
	}
	if reportDir != "" {
		p.Reports = reportDir
	}
	if logFile != "" {
		p.Log = logFile
	}
	return p
}

func xdg(v, home, fallback string) string {
	if v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(home, fallback)
}
