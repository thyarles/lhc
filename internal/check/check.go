package check

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/thyarles/lhc-go/internal/runner"
	"github.com/thyarles/lhc-go/internal/state"
)

// Meta describes a check.
type Meta struct {
	Name  string // config key under checks:, state file name, JSON id
	Title string // heading in the report
	Order int    // position in the report and in alert lists
}

// Check is one module of the health check.
type Check interface {
	Meta() Meta
	// Defaults returns a pointer to a fresh config struct holding the
	// defaults. The checks.<name> block of the YAML is decoded on top of it,
	// strictly, and the result is handed back as Env.Config.
	Defaults() Config
	// Run inspects the host. A panic is recovered by RunAll and reported as
	// a failed section; it never takes the other checks down with it.
	Run(ctx context.Context, env *Env) *Section
}

// Config is what every check's config struct satisfies, by embedding Toggle.
type Config interface{ IsEnabled() bool }

// Toggle is embedded (yaml:",inline") in every check's config struct.
type Toggle struct {
	Enabled bool `yaml:"enabled"`
}

func (t Toggle) IsEnabled() bool { return t.Enabled }

// On is the Toggle for a check that runs by default.
var On = Toggle{Enabled: true}

// Host is what the checks may know about the machine without asking it.
type Host struct {
	Label      string // the name this report is about (see host.Label)
	Kernel     string // the kernel's own hostname
	Resolved   string // what the resolver calls it; may be a shared VIP
	PkgManager string // dnf | yum | apt-get | zypper | ""
	OS         map[string]string
}

// Env is everything a check may use.
type Env struct {
	Runner runner.Runner
	State  state.Store
	Log    *slog.Logger
	Now    func() time.Time
	Host   Host
	Config Config
}

var (
	regMu    sync.Mutex
	registry = map[string]Check{}
)

// Register adds a check. Called from each check package's init().
func Register(c Check) {
	regMu.Lock()
	defer regMu.Unlock()
	name := c.Meta().Name
	if _, dup := registry[name]; dup {
		panic("check: duplicate registration of " + name)
	}
	registry[name] = c
}

// All returns the registered checks in report order.
func All() []Check {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Check, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Check) int {
		return cmp.Or(cmp.Compare(a.Meta().Order, b.Meta().Order),
			cmp.Compare(a.Meta().Name, b.Meta().Name))
	})
	return out
}

// Lookup finds a registered check by name.
func Lookup(name string) (Check, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	c, ok := registry[name]
	return c, ok
}

// Result is the outcome of one full pass.
type Result struct {
	Sections []*Section
	Overall  Status
	Alerts   []Alert
}

// PerCheckTimeout bounds a single check, so one hung command cannot hold a
// scheduled run forever.
const PerCheckTimeout = 10 * time.Minute

// RunAll runs every enabled check, one after another.
//
// Sequential on purpose: running them in parallel would produce exactly the
// CPU spike the CPU check then reports. base supplies the Runner, logger,
// clock and host; each check gets its own State and Config.
func RunAll(ctx context.Context, checks []Check, cfgs map[string]Config, states *state.Dir, base Env) Result {
	var res Result
	for _, c := range checks {
		info := c.Meta()
		cfg, ok := cfgs[info.Name]
		if !ok {
			cfg = c.Defaults()
		}
		if !cfg.IsEnabled() {
			continue
		}
		env := base
		env.Config = cfg
		env.State = states.For(info.Name)
		if env.Log != nil {
			env.Log = env.Log.With("check", info.Name)
		}
		start := time.Now()
		sec := runOne(ctx, c, &env)
		if env.Log != nil {
			env.Log.Debug("check finished", "status", sec.Status, "took", time.Since(start).Round(time.Millisecond))
		}
		res.Sections = append(res.Sections, sec)
		res.Overall = Worse(res.Overall, sec.Status)
		res.Alerts = append(res.Alerts, sec.Alerts...)
	}
	return res
}

func runOne(ctx context.Context, c Check, env *Env) (sec *Section) {
	info := c.Meta()
	ctx, cancel := context.WithTimeout(ctx, PerCheckTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			if env.Log != nil {
				env.Log.Error("check panicked", "panic", r, "stack", string(debug.Stack()))
			}
			sec = Failed(info, fmt.Errorf("internal error: %v", r))
		}
	}()
	sec = c.Run(ctx, env)
	if sec == nil {
		return Failed(info, fmt.Errorf("check returned no result"))
	}
	if sec.Name == "" {
		sec.Name = info.Name
	}
	if sec.Title == "" {
		sec.Title = info.Title
	}
	return sec
}

// Failed is the section shown for a check that could not complete. A crash
// is CAUTION, not silence: a check that quietly stops reporting is how a real
// problem goes unseen for months.
func Failed(info Meta, err error) *Section {
	s := NewSection(info.Name, info.Title)
	s.Err = err
	s.Add("Error", err.Error(), Caution, Detail("this check could not complete; the others ran normally"))
	s.Alert(Caution, fmt.Sprintf("%s check failed: %v", info.Title, err))
	return s
}
