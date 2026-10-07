// Package config is lhc's YAML configuration: the schema, its defaults, and
// the strict loader. Two layers only — the defaults compiled into the
// binary, then the file. The file holds what this host wants different; a
// setting added in a new release arrives with its default without anyone
// editing the file.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/thyarles/lhc-go/internal/check"
)

// Example is the commented starter file written by `lhc config init`. A test
// asserts that loading it yields exactly Default(), so the documentation in
// it cannot drift from the code.
//
//go:embed config.example.yaml
var Example []byte

type Config struct {
	// Overrides the name this host calls itself in reports, subject lines
	// and saved report files. Blank uses the kernel hostname.
	Hostname string   `yaml:"hostname"`
	SMTP     SMTP     `yaml:"smtp"`
	Email    Email    `yaml:"email"`
	Alerts   Alerts   `yaml:"alerts"`
	Schedule Schedule `yaml:"schedule"`
	Reports  Reports  `yaml:"reports"`
	Paths    Paths    `yaml:"paths"`
	// Checks holds each registered check's decoded config, by name.
	Checks map[string]check.Config `yaml:"-"`
}

type SMTP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// none | starttls | implicit
	TLS           string `yaml:"tls"`
	TLSSkipVerify bool   `yaml:"tls_skip_verify"`
	Username      string `yaml:"username"`
	Password      string `yaml:"password"`
	// Blank = lhc@<this host's name>.
	From string `yaml:"from"`
}

type Email struct {
	// Small "the system is alive" group: always gets a message.
	DailyRecipients []string `yaml:"daily_recipients"`
	// Broad list: only hears about NEW problems worth acting on.
	AlertRecipients []string `yaml:"alert_recipients"`
	// inline | attachment | both
	HTMLMode string `yaml:"html_mode"`
}

type Alerts struct {
	// Minimum severity that reaches alert_recipients: caution | unhealthy.
	NotifyAllOn     string   `yaml:"notify_all_on"`
	RemindCaution   Duration `yaml:"remind_caution"`
	RemindUnhealthy Duration `yaml:"remind_unhealthy"`
	ForgetAfter     Duration `yaml:"forget_after"`
}

type Schedule struct {
	// HH:MM of the first run of the day.
	Time string `yaml:"time"`
	// Blank = once a day. Otherwise whole hours, e.g. 6h: 00:07, 06:07, ...
	Every Duration `yaml:"every"`
	// Wait a random slice of random_window before starting a scheduled run.
	Random bool `yaml:"random"`
	// Kept as text and read at run time: a typo falls back to 8h with a
	// warning rather than stopping the run or sending every host back to
	// starting at the same minute.
	RandomWindow string `yaml:"random_window"`
}

type Reports struct {
	// How many saved reports to keep. 0 keeps everything.
	Keep int `yaml:"keep"`
}

type Paths struct {
	StateDir  string `yaml:"state_dir"`
	ReportDir string `yaml:"report_dir"`
	LogFile   string `yaml:"log_file"`
}

// Default is the configuration a host runs with when its file says nothing.
func Default() *Config {
	c := &Config{
		SMTP:  SMTP{Host: "relay.example.com", Port: 25, TLS: "none"},
		Email: Email{DailyRecipients: []string{}, AlertRecipients: []string{}, HTMLMode: "inline"},
		Alerts: Alerts{
			// unhealthy is what the installed Python fleet actually ran.
			NotifyAllOn:     "unhealthy",
			RemindCaution:   Duration(168 * time.Hour),
			RemindUnhealthy: Duration(24 * time.Hour),
			ForgetAfter:     Duration(72 * time.Hour),
		},
		Schedule: Schedule{Time: "00:07", Random: true, RandomWindow: "8h"},
		Reports:  Reports{Keep: 30},
		Checks:   map[string]check.Config{},
	}
	for _, ch := range check.All() {
		c.Checks[ch.Meta().Name] = ch.Defaults()
	}
	return c
}

// Check returns the config of one check; defaults if it is not registered.
func (c *Config) Check(name string) check.Config {
	if cc, ok := c.Checks[name]; ok {
		return cc
	}
	if ch, ok := check.Lookup(name); ok {
		return ch.Defaults()
	}
	return check.Toggle{}
}

// file mirrors Config for decoding, with the checks block kept raw so each
// block can be decoded into its own check's struct.
type file struct {
	Hostname string               `yaml:"hostname"`
	SMTP     *SMTP                `yaml:"smtp"`
	Email    *Email               `yaml:"email"`
	Alerts   *Alerts              `yaml:"alerts"`
	Schedule *Schedule            `yaml:"schedule"`
	Reports  *Reports             `yaml:"reports"`
	Paths    *Paths               `yaml:"paths"`
	Checks   map[string]yaml.Node `yaml:"checks"`
}

// Parse decodes YAML on top of the defaults. Unknown keys are errors, so a
// misspelt setting fails loudly instead of silently doing nothing.
func Parse(b []byte) (*Config, error) {
	c := Default()
	f := file{
		Hostname: c.Hostname, SMTP: &c.SMTP, Email: &c.Email, Alerts: &c.Alerts,
		Schedule: &c.Schedule, Reports: &c.Reports, Paths: &c.Paths,
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	c.Hostname = f.Hostname
	names := make([]string, 0, len(f.Checks))
	for name := range f.Checks {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		node := f.Checks[name]
		ch, ok := check.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("line %d: checks.%s: no such check (known: %s)", node.Line, name, strings.Join(Known(), ", "))
		}
		cc := ch.Defaults()
		if err := decodeStrict(&node, cc); err != nil {
			return nil, fmt.Errorf("checks.%s: %w", name, err)
		}
		c.Checks[name] = cc
	}
	return c, nil
}

// yaml.v3 has no strict mode for Node.Decode, so round-trip through bytes.
func decodeStrict(n *yaml.Node, out any) error {
	if n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return nil
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Known lists the registered check names.
func Known() []string {
	var out []string
	for _, ch := range check.All() {
		out = append(out, ch.Meta().Name)
	}
	return out
}

// Load reads path. A missing file is not an error: the defaults apply and
// found is false.
func Load(path string) (c *Config, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	c, err = Parse(b)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	return c, true, nil
}

var hhmm = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)

// ParseTime reads HH:MM.
func ParseTime(v string) (h, m int, err error) {
	g := hhmm.FindStringSubmatch(strings.TrimSpace(v))
	if g == nil {
		return 0, 0, fmt.Errorf("invalid time %q: use HH:MM, e.g. 00:07", v)
	}
	_, err = fmt.Sscanf(g[1]+" "+g[2], "%d %d", &h, &m)
	return h, m, err
}

// Validate checks the values the schema cannot express.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch c.SMTP.TLS {
	case "none", "starttls", "implicit":
	default:
		bad("smtp.tls: %q is not one of none, starttls, implicit", c.SMTP.TLS)
	}
	if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
		bad("smtp.port: %d is out of range", c.SMTP.Port)
	}
	if c.SMTP.Username != "" && c.SMTP.TLS == "none" {
		bad("smtp.username is set but smtp.tls is none: refusing to send a password in clear text (use starttls or implicit)")
	}
	switch c.Email.HTMLMode {
	case "inline", "attachment", "both":
	default:
		bad("email.html_mode: %q is not one of inline, attachment, both", c.Email.HTMLMode)
	}
	for _, list := range []struct {
		key  string
		addr []string
	}{{"email.daily_recipients", c.Email.DailyRecipients}, {"email.alert_recipients", c.Email.AlertRecipients}} {
		for _, a := range list.addr {
			if !strings.Contains(a, "@") || strings.ContainsAny(a, " ,;<>") {
				bad("%s: %q is not an e-mail address", list.key, a)
			}
		}
	}
	switch c.Alerts.NotifyAllOn {
	case "caution", "unhealthy":
	default:
		bad("alerts.notify_all_on: %q is not one of caution, unhealthy", c.Alerts.NotifyAllOn)
	}
	if _, _, err := ParseTime(c.Schedule.Time); err != nil {
		bad("schedule.time: %v", err)
	}
	if e := c.Schedule.Every.D(); e != 0 {
		if e < time.Hour || e > 24*time.Hour || e%time.Hour != 0 {
			bad("schedule.every: %s must be whole hours between 1h and 24h", c.Schedule.Every)
		}
	}
	if _, err := ParseDuration(c.Schedule.RandomWindow); err != nil {
		bad("schedule.random_window: %v", err)
	}
	if c.Reports.Keep < 0 {
		bad("reports.keep: must not be negative")
	}
	for name, cc := range c.Checks {
		if v, ok := cc.(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				bad("checks.%s: %v", name, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Marshal renders the effective config, checks included, as YAML.
func (c *Config) Marshal() ([]byte, error) {
	type out struct {
		Hostname string                  `yaml:"hostname"`
		SMTP     SMTP                    `yaml:"smtp"`
		Email    Email                   `yaml:"email"`
		Alerts   Alerts                  `yaml:"alerts"`
		Schedule Schedule                `yaml:"schedule"`
		Reports  Reports                 `yaml:"reports"`
		Paths    Paths                   `yaml:"paths"`
		Checks   map[string]check.Config `yaml:"checks"`
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	err := enc.Encode(out{c.Hostname, c.SMTP, c.Email, c.Alerts, c.Schedule, c.Reports, c.Paths, c.Checks})
	return buf.Bytes(), err
}
