package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc-go/internal/alerts"
	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/notify"
	"github.com/thyarles/lhc-go/internal/notify/smtp"
	"github.com/thyarles/lhc-go/internal/report"
	"github.com/thyarles/lhc-go/internal/schedule"
	"github.com/thyarles/lhc-go/internal/state"
	"github.com/thyarles/lhc-go/internal/version"
)

// runOpts selects what one pass does.
type runOpts struct {
	scheduled bool   // started by the timer/cron: log to the file, honour the delay
	noRandom  bool   // skip the random delay (systemd already applied its own)
	dryRun    bool   // frozen state, no mail, no saved report
	deliver   bool   // save the report and send mail
	format    string // what to print on stdout: "", text, html, json
}

func newRunCmd(a *app) *cobra.Command {
	var o runOpts
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run every check, save the report and send the mail",
		Long: `Run every enabled check, save the report, and mail it.

The daily list gets every run's report; the alert list only hears about
conditions it has not already been told about. --dry-run prints the report
instead and changes nothing: no mail, no saved report, no baselines consumed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(o.format, false); err != nil {
				return err
			}
			o.deliver = !o.dryRun
			if o.dryRun && o.format == "" {
				o.format = "text"
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := a.load(); err != nil {
				return err
			}
			return a.pass(ctx, o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.scheduled, "scheduled", false, "started by the scheduler: write to the log file and honour schedule.random")
	f.BoolVar(&o.noRandom, "no-random", false, "do not wait the random delay (the systemd timer already did)")
	f.BoolVar(&o.dryRun, "dry-run", false, "print the report; send nothing, save nothing, change no state")
	f.StringVar(&o.format, "format", "", "also print the report: text or json")
	return cmd
}

func newReportCmd(a *app) *cobra.Command {
	format := "text"
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Print a report now without sending mail or changing state",
		Long: `Run the checks and print the report. Nothing is mailed or saved, and the
baselines the scheduled run compares against are left untouched, so a
preview never hides tomorrow's "new port" or "new SUID file".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format, true); err != nil {
				return err
			}
			if err := a.load(); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.pass(ctx, runOpts{dryRun: true, format: format})
		},
	}
	cmd.Flags().StringVar(&format, "format", format, "text, html or json")
	return cmd
}

func checkFormat(f string, allowHTML bool) error {
	switch f {
	case "", "text", "json":
		return nil
	case "html":
		if allowHTML {
			return nil
		}
	}
	return fmt.Errorf("unknown --format %q", f)
}

// pass is one full run: checks, alert triage, report, delivery.
func (a *app) pass(ctx context.Context, o runOpts) error {
	progress := a.stderr
	if o.scheduled {
		// A scheduled run writes its own log: cron would otherwise mail the
		// output to root, and the delay announcement must land somewhere a
		// human can tail.
		f, err := openLog(a.paths.Log)
		if err != nil {
			_, _ = fmt.Fprintf(a.stderr, "lhc: log file %s: %v (logging to stderr)\n", a.paths.Log, err)
		} else {
			defer func() { _ = f.Close() }()
			progress = f
		}
	}
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(progress, format+"\n", args...) }
	log := a.logger(progress)

	if !a.cfgFound {
		say("  ! no config at %s — using defaults (create one with `lhc config init`)", a.paths.Config)
	}

	// Before any measurement: the delay exists to move the LOAD off the
	// hour, so taking it after the checks would defeat the point.
	if o.scheduled && !o.noRandom {
		schedule.Delay(ctx, a.cfg.Schedule, progress, a.now)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	if o.deliver {
		unlock, err := state.Lock(a.paths.State)
		if errors.Is(err, state.ErrLocked) {
			say("  ! %v — skipping this run", err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("state directory %s: %w", a.paths.State, err)
		}
		defer unlock()
	}

	h := a.hostInfo()
	now := a.now()
	say("[%s] Running health checks on %s...", now.Format("2006-01-02 15:04:05"), h.Label)

	states := &state.Dir{Path: a.paths.State, ReadOnly: !o.deliver, Now: a.now}
	base := check.Env{Runner: a.runner, Log: log, Now: a.now, Host: h}
	res := check.RunAll(ctx, check.All(), a.cfg.Checks, states, base)
	say("  Overall status: %s", res.Overall.Word())

	policy := alerts.Policy{
		RemindCaution:   a.cfg.Alerts.RemindCaution.D(),
		RemindUnhealthy: a.cfg.Alerts.RemindUnhealthy.D(),
		ForgetAfter:     a.cfg.Alerts.ForgetAfter.D(),
	}
	policy.NotifyAllOn, _ = check.ParseStatus(a.cfg.Alerts.NotifyAllOn)
	decision := alerts.Evaluate(res.Alerts, states.For("alerts"), policy, now)
	say("  Alert triage: %s", decision.Summary())

	m := report.Build(res, decision, h.Label, version.Version, now)
	text := report.Text(m)
	html, err := report.HTML(m)
	if err != nil {
		return err
	}
	if err := a.print(o.format, m, text, html); err != nil {
		return err
	}
	if !o.deliver {
		return nil
	}

	if path, err := saveReport(a.paths.Reports, h.Label, now, text, html, a.cfg.Reports.Keep); err != nil {
		say("  ! could not save the report: %v", err)
	} else {
		say("  Report saved: %s", path)
	}

	to, isAlert := notify.PlanDelivery(decision, a.cfg.Email.DailyRecipients, a.cfg.Email.AlertRecipients)
	subject := notify.Subject(res.Overall, decision, h.Label, isAlert, now)
	say("  Subject: %s", subject)
	switch {
	case isAlert:
		say("  Alerting the broad list — %s", decision.Reason)
	case decision.NotifyAll && len(a.cfg.Email.AlertRecipients) == 0:
		say("  ! Findings need attention but no alert_recipients are configured — heartbeat only")
	default:
		say("  No broad alert — %s", decision.Reason)
	}

	if len(to) == 0 {
		say("  No recipients configured — nothing sent (set email.daily_recipients)")
		// Nothing was lost: there was nobody to tell. Commit normally.
		return decision.Commit()
	}
	sender := &smtp.Sender{Config: smtp.Config{
		Host: a.cfg.SMTP.Host, Port: a.cfg.SMTP.Port, TLS: a.cfg.SMTP.TLS, TLSSkipVerify: a.cfg.SMTP.TLSSkipVerify,
		Username: a.cfg.SMTP.Username, Password: a.cfg.SMTP.Password, HelloName: h.Label,
	}, Now: a.now}
	msg := notify.Message{
		From: a.fromAddress(h), To: to, Subject: subject, Text: text, HTML: html, HTMLMode: a.cfg.Email.HTMLMode,
	}
	if err := sender.Send(ctx, msg); err != nil {
		// Only a delivered message marks conditions as notified. If the relay
		// was down, the next run treats them as new and tries again.
		say("  ✗ Email failed (%s:%d) → %v", a.cfg.SMTP.Host, a.cfg.SMTP.Port, err)
		say("  ! Delivery failed — alerts left unacknowledged, will retry next run")
		return &exitError{code: 2, err: fmt.Errorf("delivery failed: %w", err)}
	}
	say("  ✓ Email sent → %s", strings.Join(to, ", "))
	return decision.Commit()
}

func (a *app) print(format string, m *report.Model, text, html string) error {
	switch format {
	case "text":
		_, err := io.WriteString(a.stdout, text)
		return err
	case "html":
		_, err := io.WriteString(a.stdout, html)
		return err
	case "json":
		b, err := report.JSON(m)
		if err != nil {
			return err
		}
		_, err = a.stdout.Write(b)
		return err
	}
	return nil
}

// saveReport writes health-<host>-<YYYYmmdd-HHMM>.{txt,html} and keeps the
// newest `keep` runs. Retention matters once `schedule.every` runs several
// times a day.
func saveReport(dir, host string, now time.Time, text, html string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	stem := filepath.Join(dir, "health-"+strings.ReplaceAll(host, "/", "_")+"-"+now.Format("20060102-1504"))
	if err := state.WriteFileAtomic(stem+".txt", []byte(text), 0o640); err != nil {
		return "", err
	}
	if err := state.WriteFileAtomic(stem+".html", []byte(html), 0o640); err != nil {
		return "", err
	}
	return stem + ".txt", pruneReports(dir, keep)
}

func pruneReports(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var stems []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "health-") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(name, ".txt"), ".html")
		if len(stem) < len("health--20060102-1504") {
			continue
		}
		if !seen[stem] {
			seen[stem] = true
			stems = append(stems, stem)
		}
	}
	// The timestamp is the tail of the stem, so sort on it rather than on
	// the whole name: a host renamed mid-life still prunes oldest first.
	slices.SortFunc(stems, func(x, y string) int { return strings.Compare(x[len(x)-13:], y[len(y)-13:]) })
	var errs []error
	for _, stem := range stems[:max(0, len(stems)-keep)] {
		for _, ext := range []string{".txt", ".html"} {
			if err := os.Remove(filepath.Join(dir, stem+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// openLog opens the log for appending, rotating it to .1 past 5 MB so a
// host that runs every hour for years does not fill /var/log.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
