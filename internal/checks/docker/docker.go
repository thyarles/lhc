// Package docker reports Docker containers that crashed recently, are stuck
// restarting or fail their own healthcheck.
package docker

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	// A container that exited non-zero within this many hours is a fresh
	// crash (CAUTION); older ones are assumed stopped on purpose.
	RecentHours float64 `yaml:"recent_hours"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "docker", Title: "Docker Containers", Order: 70}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On, RecentHours: 24} }

// Validate keeps the crash window meaningful.
func (c *Config) Validate() error {
	if c.RecentHours <= 0 {
		return fmt.Errorf("recent_hours (%g) must be positive", c.RecentHours)
	}
	return nil
}

const (
	listCap = 10
	timeout = 15 * time.Second
)

var (
	agoRE   = regexp.MustCompile(`(\d+|an?)\s+(second|minute|hour|day|week|month|year)s?\s+ago`)
	agoHour = map[string]float64{"second": 1.0 / 3600, "minute": 1.0 / 60, "hour": 1, "day": 24,
		"week": 168, "month": 730, "year": 8760}
	exitRE = regexp.MustCompile(`exited \((\d+)\)`)
)

// statusAgeHours reads the age from Docker's humanised status ("Exited (1)
// 3 days ago"). A status with no age at all reads as ancient.
func statusAgeHours(status string) float64 {
	m := agoRE.FindStringSubmatch(status)
	if m == nil {
		if strings.Contains(strings.ToLower(status), "less than") {
			return 0
		}
		return 1e9
	}
	qty := 1.0
	if m[1] != "a" && m[1] != "an" {
		qty, _ = strconv.ParseFloat(m[1], 64)
	}
	h, ok := agoHour[m[2]]
	if !ok {
		h = 1
	}
	return qty * h
}

type container struct {
	name, status, image string
	st                  check.Status
}

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("docker", c.Meta().Title)
	r := env.Runner

	if _, ok := r.LookPath("docker"); !ok {
		s.NotApplicable("Not installed (optional)")
		s.NeedTool(check.Tool{Name: "docker", RHELPkg: "docker-ce", DebPkg: "docker.io", SUSEPkg: "docker", Optional: true})
		return s
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := r.Run(ctx, "docker", "ps", "-a", "--format", "{{.Names}}\t{{.Status}}\t{{.Image}}")
	if res.Code != 0 || res.Err != nil {
		// Installed but stopped (a host where Docker came along with a
		// package and was never enabled) is not a container problem, and
		// saying "None" would be a claim we cannot make.
		if strings.Contains(res.Stderr, "Cannot connect to the Docker daemon") {
			s.NotApplicable("Installed, but the daemon is not running")
			return s
		}
		why := strings.TrimSpace(strings.SplitN(res.Stderr, "\n", 2)[0])
		if why == "" {
			why = "exit status " + strconv.Itoa(res.Code)
		}
		s.Add("Containers", "Could not be listed", check.Info, check.Detail(why))
		return s
	}
	if res.Stdout == "" {
		s.Add("Containers", "None", check.Info)
		return s
	}

	var problems []string
	var healthy []container
	stopped := 0
	for _, line := range strings.Split(res.Stdout, "\n") {
		parts := strings.Split(line, "\t")
		get := func(n int) string {
			if len(parts) > n {
				return parts[n]
			}
			return "?"
		}
		name, status, image := get(0), get(1), get(2)
		low := strings.ToLower(status)

		// A container that exited cleanly, or that has been down for weeks,
		// was almost certainly stopped on purpose: batch jobs, k8s init pods,
		// old project stacks. Flagging those daily is what trains people to
		// ignore the report. Only a fresh crash or a restart loop needs
		// attention.
		exitCode := -1
		if m := exitRE.FindStringSubmatch(low); m != nil {
			exitCode, _ = strconv.Atoi(m[1])
		}
		var st check.Status
		note := ""
		switch {
		case strings.HasPrefix(low, "up"):
			st = check.OK
			if strings.Contains(low, "(unhealthy)") {
				st, note = check.Caution, "container reports unhealthy"
			}
		case strings.HasPrefix(low, "restarting"):
			st, note = check.Caution, "restart loop"
		case exitCode > 0:
			stopped++
			if statusAgeHours(status) <= cfg.RecentHours {
				st, note = check.Caution, fmt.Sprintf("crashed within the last %.0fh", cfg.RecentHours)
			} else {
				st, note = check.Info, "stopped (long ago — assumed intentional)"
			}
		default: // exited (0), created, paused
			stopped++
			st = check.Info
		}

		if st == check.Caution {
			problems = append(problems, fmt.Sprintf("%s (%s)", name, note))
			s.Add(name, status+"  ["+image+"]", st, check.Detail(note))
		} else {
			healthy = append(healthy, container{name, status, image, st})
		}
	}

	// Containers that are fine are a count, not a wall. On a host running
	// Kubernetes this section was 54 lines of pod names nobody reads.
	running := 0
	for _, h := range healthy {
		if h.st == check.OK {
			running++
		}
	}
	s.Add("Running", fmt.Sprintf("%d container(s) up", running), check.OK)
	for i, h := range healthy {
		if i == listCap {
			s.Add("…", fmt.Sprintf("and %d more container(s)", len(healthy)-listCap), check.Info)
			break
		}
		s.Add(h.name, h.status+"  ["+h.image+"]", h.st)
	}
	s.Add("Stopped/Exited Containers", strconv.Itoa(stopped), check.Info)
	if len(problems) > 0 {
		msg := "Docker: " + strings.Join(problems[:min(3, len(problems))], "; ")
		if len(problems) > 3 {
			msg += fmt.Sprintf(" (+%d more)", len(problems)-3)
		}
		s.Alert(check.Caution, msg)
	}

	diskUsage(ctx, s, env)
	return s
}

// diskUsage adds one row per kind (Images, Containers, Local Volumes, Build
// Cache). A Go template rather than the table, whose two-word type names
// cannot be told apart from the columns by splitting on spaces.
func diskUsage(ctx context.Context, s *check.Section, env *check.Env) {
	out := env.Runner.Run(ctx, "docker", "system", "df", "--format",
		"{{.Type}}\t{{.TotalCount}}\t{{.Active}}\t{{.Size}}\t{{.Reclaimable}}").Stdout
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.Split(line, "\t"); len(p) == 5 {
			rows = append(rows, p)
		}
	}
	if len(rows) == 0 {
		return
	}
	s.Separator("Docker Disk Usage")
	for _, p := range rows {
		s.Add(p[0], fmt.Sprintf("%s total, %s active, %s (reclaimable %s)", p[1], p[2], p[3], p[4]), check.Info)
	}
}
