// Package disk reports usage per filesystem, once per filesystem, with the
// change since the previous run.
package disk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/thyarles/lhc-go/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	Caution      float64 `yaml:"caution"`
	Unhealthy    float64 `yaml:"unhealthy"`
	// Ignore is added to DefaultIgnore, never a replacement for it.
	Ignore []string `yaml:"ignore"`
}

type Check struct{}

func (Check) Meta() check.Meta { return check.Meta{Name: "disk", Title: "Disk Usage", Order: 40} }

func (Check) Defaults() check.Config {
	return &Config{Toggle: check.On, Caution: 90, Unhealthy: 95, Ignore: []string{}}
}

// Validate keeps the thresholds in order and rejects a glob that cannot be
// compiled, so a typo fails `lhc config validate` instead of every run.
func (c *Config) Validate() error {
	var errs []error
	if c.Caution > c.Unhealthy {
		errs = append(errs, fmt.Errorf("caution (%g) is above unhealthy (%g)", c.Caution, c.Unhealthy))
	}
	for _, g := range c.Ignore {
		if _, err := compileGlob(strings.TrimSpace(g)); err != nil {
			errs = append(errs, fmt.Errorf("ignore: %q is not a valid pattern: %w", g, err))
		}
	}
	return errors.Join(errs...)
}

// DefaultIgnore lists mount points a container runtime creates that are not
// this host's storage. The patterns are fnmatch-style, and fnmatch's '*'
// crosses '/' — the opposite of shell globbing — which is why one trailing
// '*' covers a whole subtree. The trailing '/*' also matters: it skips the
// per-container mounts *under* a path while still reporting a dedicated
// filesystem mounted at the path itself, which is the one worth watching.
var DefaultIgnore = []string{
	"/var/lib/kubelet/pods/*",    // subPath / local-volume / emptyDir binds
	"/var/lib/kubelet/plugins/*", // CSI globalmount stage dirs
	"/var/lib/kubelet/plugins_registry/*",
	"/run/k3s/containerd/*", // k3s AND rke2 sandbox shm + rootfs
	"/var/lib/rancher/k3s/agent/containerd/*",
	"/var/lib/rancher/rke2/agent/containerd/*",
	"/run/containerd/*",
	"/var/lib/containerd/*",
	"/var/lib/containers/storage/*", // podman / CRI-O
	"/var/lib/docker/containers/*",
	"/var/lib/docker/overlay2/*",
	"/var/lib/docker/plugins/*",
	"/run/netns/*", "/var/run/netns/*", "/run/docker/netns/*",
	"/snap/*", "/var/snap/*",
}

// compileGlob translates a pattern with Python's fnmatch semantics into an
// anchored regexp: '*' matches any run of characters INCLUDING '/', '?' any
// one character, '[seq]' / '[!seq]' a set; a '[' with no closing ']' is
// literal. path.Match cannot be used: its '*' stops at '/'.
func compileGlob(pat string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`^(?s:`)
	rs := []rune(pat)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '*':
			for i+1 < len(rs) && rs[i+1] == '*' {
				i++
			}
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '[':
			j := i + 1
			if j < len(rs) && rs[j] == '!' {
				j++
			}
			if j < len(rs) && rs[j] == ']' {
				j++
			}
			for j < len(rs) && rs[j] != ']' {
				j++
			}
			if j >= len(rs) {
				b.WriteString(`\[`)
				continue
			}
			set := rs[i+1 : j]
			i = j
			b.WriteString(classOf(set))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)$`)
	return regexp.Compile(b.String())
}

// classOf renders the inside of a '[...]' as a regexp class. The scan in
// compileGlob guarantees the set is non-empty even after a leading '!'.
func classOf(set []rune) string {
	neg := set[0] == '!'
	if neg {
		set = set[1:]
	}
	var b strings.Builder
	b.WriteString("[")
	if neg {
		b.WriteString("^")
	}
	for _, r := range set {
		switch r {
		case '\\', '[', ']', '^':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	b.WriteString("]")
	return b.String()
}

// ignoreSet compiles the built-in globs plus the configured extras. A bad
// extra is skipped (Validate reports it); the built-ins always apply.
func ignoreSet(extra []string, warn func(string, error)) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, g := range append(append([]string{}, DefaultIgnore...), extra...) {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		re, err := compileGlob(g)
		if err != nil {
			warn(g, err)
			continue
		}
		out = append(out, re)
	}
	return out
}

func level(v, caution, unhealthy float64) check.Status {
	switch {
	case v >= unhealthy:
		return check.Unhealthy
	case v >= caution:
		return check.Caution
	}
	return check.OK
}

type mount struct {
	device            string
	size, used, avail int64
	pct               float64
	path              string
}

type fsKey struct {
	device            string
	size, used, avail int64
}

// pseudoDevice reports df lines that are not storage. 'shm' and 'nsfs' are
// named devices, not types: a pod sandbox's 64MB shm mount reports the
// device 'shm', so filtering on 'tmpfs' alone never caught it.
func pseudoDevice(dev string) bool {
	switch dev {
	case "shm", "nsfs":
		return true
	}
	for _, k := range []string{"tmpfs", "udev", "overlay", "squashfs"} {
		if strings.Contains(dev, k) {
			return true
		}
	}
	return false
}

// parseDF reads `df -Pk`, dropping pseudo filesystems and ignored mounts.
func parseDF(out string, ignore []*regexp.Regexp) []mount {
	var rows []mount
	for _, line := range strings.Split(out, "\n") {
		p := strings.Fields(line)
		if len(p) < 6 || p[0] == "Filesystem" || pseudoDevice(p[0]) {
			continue
		}
		// -P keeps one filesystem per line; a mount point with spaces is
		// still split by Fields, so put it back together.
		path := strings.Join(p[5:], " ")
		if ignored(path, ignore) {
			continue
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(p[4], "%"), 64)
		size, err2 := strconv.ParseInt(p[1], 10, 64)
		used, err3 := strconv.ParseInt(p[2], 10, 64)
		avail, err4 := strconv.ParseInt(p[3], 10, 64)
		if err != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		rows = append(rows, mount{p[0], size * 1024, used * 1024, avail * 1024, pct, path})
	}
	return rows
}

func ignored(path string, globs []*regexp.Regexp) bool {
	for _, re := range globs {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// dedupe keeps one row per filesystem. A bind mount reports the SAME device,
// size, used and free as its source. One filesystem is one fact: forty pods
// bind-mounting subPaths off / must not produce forty identical rows — nor,
// when / crosses 90%, forty identical alerts, each with its own fingerprint.
// The shortest mount path wins, so '/' always survives; order is kept.
func dedupe(rows []mount) (kept []mount, hidden int) {
	best := map[fsKey]int{}
	for i, r := range rows {
		k := fsKey{r.device, r.size, r.used, r.avail}
		if j, ok := best[k]; !ok || len(r.path) < len(rows[j].path) {
			best[k] = i
		}
	}
	for i, r := range rows {
		if best[fsKey{r.device, r.size, r.used, r.avail}] != i {
			hidden++
			continue
		}
		kept = append(kept, r)
	}
	return kept, hidden
}

const stateKey = "disk_pct"

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("disk", c.Meta().Title)
	var prev map[string]float64
	if !env.State.Load(stateKey, &prev) {
		prev = nil
	}
	current := map[string]float64{}
	globs := ignoreSet(cfg.Ignore, func(g string, err error) {
		env.Log.Warn("skipping invalid disk ignore pattern", "pattern", g, "err", err)
	})

	// The exit status is not consulted: df exits 1 when one mount (a stale
	// NFS, a FUSE mount owned by another user) cannot be read, and still
	// prints every other filesystem.
	rows, hidden := dedupe(parseDF(env.Runner.Run(ctx, "df", "-Pk").Stdout, globs))
	for _, r := range rows {
		st := level(r.pct, cfg.Caution, cfg.Unhealthy)
		if st.Flagged() {
			s.Alert(st, fmt.Sprintf("Disk %s at %.0f%%", r.path, r.pct))
		}
		current[r.path] = math.Round(r.pct*10) / 10
		s.Add(r.path,
			fmt.Sprintf("%.1f%% used  (%s of %s, %s free)", r.pct, fmtBytes(r.used), fmtBytes(r.size), fmtBytes(r.avail)),
			st, check.Meter(r.pct), check.Delta(deltaNote(prev, r.path, r.pct)))
	}
	if hidden > 0 {
		s.Add("Hidden Mounts", fmt.Sprintf("%d bind/container mount(s) on filesystems already listed", hidden),
			check.Info, check.Detail("adjust with checks.disk.ignore in the config"))
	}

	if err := env.State.Save(stateKey, current); err != nil {
		env.Log.Warn("saving disk usage", "err", err)
	}
	return s
}

// deltaNote is the signed change since the previous run. A percentage on its
// own does not say whether you have days or hours. Moves under half a point
// are noise and leave the column empty: silence is the message.
func deltaNote(prev map[string]float64, key string, now float64) string {
	before, ok := prev[key]
	if !ok {
		return ""
	}
	diff := now - before
	if math.Abs(diff) < 0.5 {
		return ""
	}
	return fmt.Sprintf("%+.1f pts since last run", diff)
}

// fmtBytes matches the Python report: integer-divide by 1024 until the value
// fits, then print it with no decimals ("1 GB", not "1.5 GB").
func fmtBytes(n int64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if n > -1024 && n < 1024 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		n = floorDiv(n, 1024)
	}
	return fmt.Sprintf("%d PB", n)
}

// floorDiv is Python's //, which rounds towards minus infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
