// Package updates counts pending package updates, and security updates where
// the package manager can tell them apart.
package updates

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/runner"
)

func init() { check.Register(Check{}) }

// Pending updates are planned maintenance, not an incident. Both thresholds
// default to 0, which means "report as INFO"; set them to make a patching
// backlog actually page people.
type Config struct {
	check.Toggle    `yaml:",inline"`
	Caution         int `yaml:"caution"`
	SecurityCaution int `yaml:"security_caution"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "updates", Title: "Pending Updates", Order: 90}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

// Validate rejects negative thresholds.
func (c *Config) Validate() error {
	if c.Caution < 0 || c.SecurityCaution < 0 {
		return fmt.Errorf("caution and security_caution must be 0 (off) or positive")
	}
	return nil
}

// Refreshing metadata and resolving updates reaches the mirrors; a slow
// mirror is common, a hung one should not hold the run.
const timeout = 90 * time.Second

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("updates", c.Meta().Title)
	r := env.Runner
	pm := env.Host.PkgManager

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch pm {
	case "dnf", "yum":
		// check-update exits 100 when updates exist; that is the answer, not
		// an error. 1 (or a timeout) means the repositories were unreachable.
		res := r.Run(ctx, pm, "check-update", "-q")
		if failed(res, 0, 100) {
			unknown(s, res)
			return s
		}
		total(s, cfg, countRPM(res.Stdout))

	case "apt-get":
		r.Run(ctx, "apt-get", "update", "-qq")
		res := r.Run(ctx, "apt-get", "-s", "upgrade")
		if failed(res, 0) {
			unknown(s, res)
			return s
		}
		all, sec := countApt(res.Stdout)
		total(s, cfg, all)
		security(s, cfg, sec)

	case "zypper":
		res := r.Run(ctx, "zypper", "--non-interactive", "--quiet", "list-updates")
		if failed(res, 0) {
			unknown(s, res)
			return s
		}
		total(s, cfg, countZypperUpdates(res.Stdout))
		res = r.Run(ctx, "zypper", "--non-interactive", "--quiet", "list-patches", "--category", "security")
		if !failed(res, 0) {
			security(s, cfg, countZypperPatches(res.Stdout))
		}

	default:
		s.Add("Package Manager", "Not detected", check.Info)
	}
	return s
}

func failed(res runner.Result, okCodes ...int) bool {
	if res.Err != nil || res.Code < 0 {
		return true
	}
	for _, c := range okCodes {
		if res.Code == c {
			return false
		}
	}
	return true
}

// unknown reports a query that failed. Saying "0" there would claim the
// host is fully patched when it simply could not reach its mirrors.
func unknown(s *check.Section, res runner.Result) {
	why := strings.TrimSpace(strings.SplitN(res.Stderr, "\n", 2)[0])
	if why == "" {
		why = "exit status " + strconv.Itoa(res.Code)
	}
	s.Add("Pending Updates", "Could not be determined", check.Info, check.Detail(why))
}

func total(s *check.Section, cfg *Config, n int) {
	st := check.OK
	switch {
	case cfg.Caution > 0 && n >= cfg.Caution:
		st = check.Caution
		s.Alert(check.Caution, fmt.Sprintf("%d pending package updates", n))
	case n > 0:
		st = check.Info
	}
	s.Add("Pending Updates", strconv.Itoa(n), st)
}

func security(s *check.Section, cfg *Config, n int) {
	if n == 0 {
		return
	}
	st := check.Info
	if cfg.SecurityCaution > 0 && n >= cfg.SecurityCaution {
		st = check.Caution
	}
	s.Add("Security Updates", strconv.Itoa(n), st)
	if st == check.Caution {
		s.Alert(check.Caution, fmt.Sprintf("%d pending security updates", n))
	}
}

// countRPM counts the package lines of `dnf/yum check-update -q`, skipping
// metadata chatter and the "Security: … is an installed security update"
// notices. Indented lines are a wrapped long name continuing the line
// above, or the package an "Obsoleting" entry replaces, never a separate
// update.
func countRPM(out string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" || l[0] == ' ' || l[0] == '\t' {
			continue
		}
		skip := false
		for _, p := range []string{"Last", "Loaded", "Loading", "Obsoleting", "Security:"} {
			if strings.HasPrefix(l, p) {
				skip = true
				break
			}
		}
		if !skip {
			n++
		}
	}
	return n
}

// countApt reads one `apt-get -s upgrade` simulation for both numbers.
//
// The old query was a case-insensitive grep for "security" over the whole
// output, which matched both the Inst and the Conf line of every package
// and reported exactly double the real number. Count Inst lines only, and
// for security only those coming from a -security archive.
func countApt(out string) (all, sec int) {
	for _, l := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(l, "Inst ")
		if !ok {
			continue
		}
		all++
		if strings.Contains(rest, "-security") {
			sec++
		}
	}
	return all, sec
}

// countZypperUpdates counts the table rows of `zypper list-updates`; every
// update row starts with the status column "v".
func countZypperUpdates(out string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "v |") {
			n++
		}
	}
	return n
}

// countZypperPatches counts the rows of the `zypper list-patches` table: the
// lines with columns that come after the "---+---" rule under the header.
// Summary lines ("Found 3 applicable patches") carry no "|".
func countZypperPatches(out string) int {
	n, body := 0, false
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "-") && strings.Contains(t, "+"):
			body = true
		case body && strings.Contains(t, "|"):
			n++
		}
	}
	return n
}
