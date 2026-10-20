package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/config"
	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/notify"
	"github.com/thyarles/lhc/internal/report"
	"github.com/thyarles/lhc/internal/version"
)

func newHubDigestCmd(a *app) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Mail the digest of every host's latest report now (--dry-run prints it)",
		Long: `Build the digest a collector mails every day at collector.digest_time
from the reports it holds, and mail it now, or print it with --dry-run.

A digest sent by hand does not count as the day's digest: the scheduled
one still goes out, and still compares with the previous scheduled one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			store := a.hubStore()
			d, _, err := a.buildDigest(store)
			if err != nil {
				return err
			}
			if dryRun {
				_, err := a.stdout.Write([]byte(report.DigestText(d)))
				return err
			}
			to, err := a.mailDigest(cmd.Context(), d)
			if err != nil {
				return err
			}
			a.printf("  ✓ Digest sent → %s", strings.Join(to, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the digest instead of mailing it")
	return cmd
}

func (a *app) hubStore() hub.Store { return hub.Store{Dir: filepath.Join(a.paths.State, "hub")} }

// buildDigest summarises the store as this collector sees it now.
func (a *app) buildDigest(store hub.Store) (*report.Digest, hub.DigestState, error) {
	entries, err := store.List()
	if err != nil {
		return nil, hub.DigestState{}, err
	}
	if len(entries) == 0 {
		return nil, hub.DigestState{}, hub.ErrNoDigestYet
	}
	d, next := hub.BuildDigest(entries, store.LoadDigest(), hub.DigestOptions{
		Self:        a.hostInfo().Label,
		Peers:       a.cfg.Collector.Peers,
		SilentAfter: a.cfg.Collector.SilentAfter.D(),
		Version:     version.Version,
		Now:         a.now(),
	})
	return d, next, nil
}

var errNoDigestRecipients = errors.New("no recipients: set collector.digest_recipients or email.daily_recipients")

// mailDigest sends the digest with the host report's SMTP settings.
func (a *app) mailDigest(ctx context.Context, d *report.Digest) ([]string, error) {
	to := a.cfg.Collector.DigestRecipients
	if len(to) == 0 {
		to = a.cfg.Email.DailyRecipients
	}
	if len(to) == 0 {
		return nil, errNoDigestRecipients
	}
	body, err := report.DigestHTML(d)
	if err != nil {
		return nil, err
	}
	h := a.hostInfo()
	return to, a.sender(h).Send(ctx, notify.Message{
		From: a.fromAddress(h), To: to, Subject: report.DigestSubject(d),
		Text: report.DigestText(d), HTML: body, HTMLMode: a.cfg.Email.HTMLMode,
	})
}

// digester sends the scheduled digest from inside `lhc collect`.
type digester struct {
	a       *app
	store   hub.Store
	logf    func(string, ...any)
	sent    time.Time // slot of the last digest sent
	lastTry time.Time
}

func newDigester(a *app, store hub.Store, logf func(string, ...any)) *digester {
	g := &digester{a: a, store: store, logf: logf, sent: store.LoadDigest().Slot}
	if g.sent.IsZero() {
		// A new collector waits for the next digest_time: a digest the
		// minute it is installed would compare with nothing and surprise.
		if h, m, err := config.ParseTime(a.cfg.Collector.DigestTime); err == nil {
			g.sent = hub.Slot(a.now(), h, m)
		}
	}
	return g
}

// tick sends the digest when it is due. config.Validate has checked the
// time, and a SIGHUP may have changed it, so it is read every tick.
func (g *digester) tick(ctx context.Context) {
	h, m, err := config.ParseTime(g.a.cfg.Collector.DigestTime)
	if err != nil {
		return
	}
	now := g.a.now()
	slot := hub.Slot(now, h, m)
	if !hub.DigestDue(now, slot, g.sent, g.lastTry) {
		return
	}
	g.lastTry = now
	d, next, err := g.a.buildDigest(g.store)
	if errors.Is(err, hub.ErrNoDigestYet) {
		g.logf("digest skipped: %v", err)
		g.sent = slot
		return
	}
	if err != nil {
		g.logf("digest not built, trying again in %s: %v", hub.RetryEvery, err)
		return
	}
	to, err := g.a.mailDigest(ctx, d)
	switch {
	case errors.Is(err, errNoDigestRecipients):
		g.logf("digest not sent: %v", err)
		g.sent = slot
		return
	case err != nil:
		g.logf("digest not sent, trying again in %s: %v", hub.RetryEvery, err)
		return
	default:
		g.logf("digest sent → %s (%s)", strings.Join(to, ", "), d.Tally())
	}
	g.sent = slot
	next.Slot = slot
	if err := g.store.SaveDigest(next); err != nil {
		g.logf("digest state not saved: %v", err)
	}
}
