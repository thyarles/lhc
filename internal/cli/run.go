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

	"github.com/thyarles/lhc/internal/alerts"
	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/notify"
	"github.com/thyarles/lhc/internal/report"
	"github.com/thyarles/lhc/internal/schedule"
	"github.com/thyarles/lhc/internal/state"
	tw "github.com/thyarles/lhc/internal/textwrap"
	"github.com/thyarles/lhc/internal/version"
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

	// A delivering run holds every baseline back until the report reached
	// someone (see commit below).
	states := &state.Dir{Path: a.paths.State, ReadOnly: !o.deliver, Deferred: o.deliver, Now: a.now}
	base := check.Env{Runner: a.runner, Log: log, Now: a.now, Host: h}
	res := check.RunAll(ctx, check.All(), a.cfg.Checks, states, base)
	// Ctrl+C, or systemd stopping the service, leaves checks half done:
	// "0 listening sockets", "nobody logged in". Such a report misleads
	// whoever reads it, and its baselines would flag every real port as new
	// tomorrow. Deferred state means returning here saves nothing.
	if ctx.Err() != nil {
		return &exitError{code: 130, err: errInterrupted}
	}
	if o.deliver && a.cfg.Collector.Enabled && a.cfg.Collector.DigestMode == "one" {
		sec := a.relaySection(ctx, h)
		res.Sections = append(res.Sections, sec)
		res.Overall = check.Worse(res.Overall, sec.Status)
		res.Alerts = append(res.Alerts, sec.Alerts...)
		say("  Digest relay: %s", sec.Rows[0].Value)
	}
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

	// Before the mail: a collector that holds the report makes the
	// heartbeat redundant. A dead one never changes how this run ends.
	outs := a.toCollectors(ctx, say, m)
	if len(outs) > 0 {
		say("  Hub: %d of %d collector(s) accepted the report", hub.Accepted(outs), len(outs))
	}
	return a.mail(ctx, say, h, res.Overall, decision, states, text, html, now, outs)
}

// mailDue says whether this run mails at all. An alert always does; the
// rest is the heartbeat, which email.heartbeat may drop: "never" always,
// "auto" only when a collector holds this run's report, so a host whose
// hub is unreachable still says it is alive.
func mailDue(alert bool, mode string, collectors, accepted int) bool {
	switch {
	case alert, mode == "always":
		return true
	case mode == "never":
		return false
	default: // auto
		return collectors == 0 || accepted == 0
	}
}

// destination is where a run below the broad alert goes. It used to say
// "daily list only" when nothing was mailed at all.
func destination(due bool, heartbeat string, recipients int) string {
	switch {
	case !due && heartbeat == "never":
		return "not mailed (email.heartbeat is never)"
	case !due:
		return "left to the hub digest"
	case recipients == 0:
		return "not mailed (no recipients)"
	default:
		return "daily list only"
	}
}

// mail sends the run's message and, once the relay accepted it, commits the
// alert history and the baselines. outs is where the hub delivery went.
func (a *app) mail(ctx context.Context, say func(string, ...any), h check.Host, overall check.Status,
	decision *alerts.Decision, states *state.Dir, text, html string, now time.Time, outs []hub.Outcome,
) error {
	to, isAlert := notify.PlanDelivery(decision, a.cfg.Email.DailyRecipients, a.cfg.Email.AlertRecipients)
	// NotifyAll, not isAlert: with no alert list the alert goes to the
	// daily list, and it is still an alert.
	due := mailDue(decision.NotifyAll, a.cfg.Email.Heartbeat, len(outs), hub.Accepted(outs))
	switch {
	case isAlert:
		say("  Alerting the broad list — %s", decision.Reason)
	case decision.NotifyAll && len(a.cfg.Email.AlertRecipients) == 0:
		say("  ! Findings need attention but no alert_recipients are configured — heartbeat only")
	default:
		say("  No broad alert — %s, %s", decision.Reason, destination(due, a.cfg.Email.Heartbeat, len(to)))
	}

	if !due {
		if a.cfg.Email.Heartbeat == "never" {
			say("  Heartbeat skipped — email.heartbeat is never")
		} else {
			say("  Heartbeat skipped — delivered to collector %s", firstAccepted(outs))
		}
		// The collector holds the report, or the owner chose no heartbeat:
		// nothing is waiting to be retried. Commit as for no recipients.
		return commit(decision, states)
	}

	if len(to) == 0 {
		say("  No recipients configured — nothing sent (set email.daily_recipients)")
		// Nothing was lost: there was nobody to tell. Commit normally.
		return commit(decision, states)
	}
	subject := notify.Subject(overall, decision, h.Label, isAlert, now)
	say("  Subject: %s", subject)
	sender := a.sender(h)
	sender.Retry = mailRetry
	sender.OnRetry = func(err error, wait time.Duration) {
		say("  ! Relay refused for now (%v) — retrying in %s", err, wait)
	}
	msg := notify.Message{
		From: a.fromAddress(h), To: to, Subject: subject, Text: text, HTML: html, HTMLMode: a.cfg.Email.HTMLMode,
	}
	if err := sender.Send(ctx, msg); err != nil {
		// Only a delivered message marks conditions as notified and moves the
		// baselines. If the relay was down, the next run diffs against the
		// last delivered state, finds the same changes and tries again.
		say("  ✗ Email failed (%s:%d) → %v", a.cfg.SMTP.Host, a.cfg.SMTP.Port, err)
		say("  ! Delivery failed — alerts left unacknowledged, will retry next run")
		return &exitError{code: 2, err: fmt.Errorf("delivery failed: %w", err)}
	}
	say("  ✓ Email sent → %s", strings.Join(to, ", "))
	return commit(decision, states)
}

// toCollectors sends the JSON report to every hub collector, one after
// another. It never fails the run: mail is the path that must always work.
// The outcomes say whether any collector holds the report.
func (a *app) toCollectors(ctx context.Context, say func(string, ...any), m *report.Model) []hub.Outcome {
	cs := a.cfg.Hub.Collectors
	if len(cs) == 0 {
		return nil
	}
	env, err := a.seal(m)
	if err != nil {
		say("  ! Hub: report not sent → %v", err)
		outs := make([]hub.Outcome, len(cs))
		for i, c := range cs {
			outs[i] = hub.Outcome{Collector: c, Err: err}
		}
		return outs
	}
	outs := hub.Send(ctx, cs, env, hub.SendTimeout)
	for _, o := range outs {
		if o.Err != nil {
			say("  ! Collector %s not reached → %v", o.Collector, o.Err)
		} else {
			say("  ✓ Report sent → collector %s", o.Collector)
		}
	}
	return outs
}

func firstAccepted(outs []hub.Outcome) string {
	for _, o := range outs {
		if o.Err == nil {
			return o.Collector
		}
	}
	return ""
}

func (a *app) seal(m *report.Model) (*hub.Envelope, error) {
	key, err := hub.LoadKey(a.cfg.Hub.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("hub.key_file: %w", err)
	}
	body, err := report.JSON(m)
	if err != nil {
		return nil, err
	}
	// Stamped now, not when the checks started: a slow run plus a mail
	// retry can take minutes, and the collector allows ten.
	return hub.Seal(key, m.Host, a.now(), body)
}

// relaySection checks, on a collector that may have to send the hub's
// digest, that it can reach the relay: connect, EHLO, TLS and AUTH as
// configured, QUIT. No mail is sent. With one digest per hub, a standby's
// mail path is otherwise only tried the day it has to take over. The alert
// has no digits: its text is its identity.
func (a *app) relaySection(ctx context.Context, h check.Host) *check.Section {
	s := check.NewSection(hub.RelaySection, "Hub Collector")
	snd := a.sender(h)
	snd.Retry = mailRetry
	if err := snd.Probe(ctx); err != nil {
		s.Add(hub.RelayRow, tw.Truncate(err.Error(), 120), check.Caution,
			check.Detail("this collector sends the hub's digest when the ones before it do not"))
		s.Alert(check.Caution, hub.RelayAlert)
		return s
	}
	s.Add(hub.RelayRow, fmt.Sprintf("reachable (%s:%d)", a.cfg.SMTP.Host, a.cfg.SMTP.Port), check.OK)
	return s
}

// errInterrupted ends a pass cut short by a signal.
var errInterrupted = errors.New("interrupted: no report printed, nothing sent or saved")

// mailRetry is how long a scheduled run waits before its one retry when the
// relay answers 4xx ("try again later"). An Exchange relay with a full queue
// disk answered 452 and accepted the same message a minute later.
const mailRetry = time.Minute

// commit records the alert history and the baselines together.
func commit(d *alerts.Decision, states *state.Dir) error {
	if err := d.Commit(); err != nil {
		return err
	}
	return states.Flush()
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
