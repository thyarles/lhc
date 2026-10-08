// Package fail2ban reports each fail2ban jail and its bans.
package fail2ban

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
	// Currently banned IPs, summed over the jails, that count as an unusual
	// spike. 0 turns the spike check off.
	BannedIPsCaution int `yaml:"banned_ips_caution"`
}

type Check struct{}

func (Check) Meta() check.Meta {
	return check.Meta{Name: "fail2ban", Title: "fail2ban", Order: 120}
}

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

// Validate rejects a negative spike threshold.
func (c *Config) Validate() error {
	if c.BannedIPsCaution < 0 {
		return fmt.Errorf("banned_ips_caution (%d) must not be negative (0 turns it off)", c.BannedIPsCaution)
	}
	return nil
}

const clientTimeout = 10 * time.Second

var (
	jailList  = regexp.MustCompile(`Jail list:\s+(.+)`)
	curBanned = regexp.MustCompile(`Currently banned:\s+(\d+)`)
	totBanned = regexp.MustCompile(`Total banned:\s+(\d+)`)
)

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	cfg := env.Config.(*Config)
	s := check.NewSection("fail2ban", c.Meta().Title)
	r := env.Runner
	// A ban is fail2ban doing its job, not a problem to escalate. Only an
	// unusual spike is worth mentioning; 0 disables that entirely.
	spike := cfg.BannedIPsCaution

	if _, ok := r.LookPath("fail2ban-client"); !ok {
		s.NotApplicable("Not installed")
		s.NeedTool(check.Tool{Name: "fail2ban-client", RHELPkg: "fail2ban", DebPkg: "fail2ban"})
		return s
	}

	out := client(ctx, env, "status")
	if !strings.Contains(out, "Jail list") {
		s.Add("fail2ban", "Service not running or no output", check.Caution)
		return s
	}
	m := jailList.FindStringSubmatch(out)
	if m == nil {
		s.Add("fail2ban", trunc(out, 120), check.Info)
		return s
	}

	total := 0
	for _, jail := range strings.Split(m[1], ",") {
		jail = strings.TrimSpace(jail)
		if jail == "" {
			continue
		}
		jout := client(ctx, env, "status", jail)
		cur, all := number(curBanned, jout), number(totBanned, jout)
		total += cur
		st := check.OK
		if cur > 0 {
			st = check.Info
		}
		s.Add("Jail: "+jail, fmt.Sprintf("%d currently banned / %d total", cur, all), st)
	}

	if spike > 0 && total >= spike {
		s.Add("Ban Volume", fmt.Sprintf("%d IPs banned (spike threshold %d)", total, spike), check.Caution)
		s.Alert(check.Caution, fmt.Sprintf("fail2ban: unusual ban volume — %d IP(s) currently banned", total))
	}
	return s
}

// client runs fail2ban-client with a short timeout: a wedged fail2ban server
// makes the client hang on its socket.
func client(ctx context.Context, env *check.Env, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	return env.Runner.Run(ctx, "fail2ban-client", args...).Stdout
}

func number(re *regexp.Regexp, out string) int {
	if m := re.FindStringSubmatch(out); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
