package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/checks/kubernetes"
	"github.com/thyarles/lhc/internal/config"
	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/kube"
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
one still goes out, and still compares with the previous scheduled one.
It is sent whatever collector.digest_mode says, and tells no other
collector; --dry-run also prints this collector's turn and today's notice.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			store := a.hubStore()
			d, _, err := a.buildDigest(cmd.Context(), store, nil)
			if err != nil {
				return err
			}
			if dryRun {
				for _, l := range a.turnLines(store) {
					a.printf("%s", l)
				}
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

// buildDigest summarises the store as this collector sees it now. t is the
// scheduled turn it is sent for; nil for one sent by hand.
func (a *app) buildDigest(ctx context.Context, store hub.Store, t *turn) (*report.Digest, hub.DigestState, error) {
	entries, err := store.List()
	if err != nil {
		return nil, hub.DigestState{}, err
	}
	if len(entries) == 0 {
		return nil, hub.DigestState{}, hub.ErrNoDigestYet
	}
	o := hub.DigestOptions{
		Self:        a.label(),
		Peers:       a.cfg.Collector.Peers,
		SilentAfter: a.cfg.Collector.SilentAfter.D(),
		Version:     version.Version,
		Now:         a.now(),
	}
	if t != nil && t.one {
		o.OnePerHub, o.Order, o.Rank, o.Slot = true, t.order, t.rank, t.slot
	}
	o.Cluster, o.LookupErr = a.lookupPods(ctx, entries)
	d, next := hub.BuildDigest(entries, store.LoadDigest(), o)
	return d, next, nil
}

// cluster is kubectl as the kubernetes check finds it on this host.
func (a *app) cluster() kube.Cluster {
	explicit := ""
	if c, ok := a.cfg.Check("kubernetes").(*kubernetes.Config); ok {
		explicit = c.Kubeconfig
	}
	return kube.Find(a.runner, explicit)
}

// lookupPods asks the cluster about every pod, once, when a host reported
// an OOM-killed pod and collector.kubernetes_lookup allows it. It is silent
// when this collector has no kubectl or kubeconfig; a failed call is
// returned for the digest's footer, never as an alert: the kubernetes
// check on this host already alerts on an unusable API.
func (a *app) lookupPods(ctx context.Context, entries []hub.Entry) (map[string]kube.Pod, string) {
	if !a.cfg.Collector.KubernetesLookup || !slices.ContainsFunc(entries, func(e hub.Entry) bool { return len(e.Pods) > 0 }) {
		return nil, ""
	}
	c := a.cluster()
	if !c.Ready() {
		return nil, ""
	}
	pods, err := c.Pods(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return pods, ""
}

// lookupLine is hub status's word on the cluster lookup.
func (a *app) lookupLine() string {
	if !a.cfg.Collector.KubernetesLookup {
		return "  Kubernetes lookup: off (collector.kubernetes_lookup: false)"
	}
	c := a.cluster()
	switch {
	case !c.HasKubectl():
		return "  Kubernetes lookup: unavailable (no kubectl)"
	case c.Kubeconfig == "":
		return "  Kubernetes lookup: unavailable (no kubeconfig)"
	}
	return "  Kubernetes lookup: on (kubeconfig " + c.Kubeconfig + ")"
}

// turn is this collector's place in the hub's digest rota.
type turn struct {
	one   bool      // collector.digest_mode one
	order []string  // the hub's collectors, sorted
	rank  int       // this one's place in order
	base  time.Time // the day's digest_time
	slot  time.Time // base plus this one's offset
	day   string    // the digest day slot belongs to
}

// turnAt is the turn whose slot was most recently reached at now:
// digest_time plus rank × takeover_after.
func (a *app) turnAt(now time.Time) (turn, error) {
	h, m, err := config.ParseTime(a.cfg.Collector.DigestTime)
	if err != nil {
		return turn{}, err
	}
	var t turn
	if a.cfg.Collector.DigestMode == "one" {
		t.one = true
		t.order, t.rank = hub.Order(a.label(), a.cfg.Collector.Peers)
	}
	off := time.Duration(t.rank) * a.cfg.Collector.TakeoverAfter.D()
	base := hub.Slot(now.Add(-off), h, m)
	t.base, t.slot, t.day = base, base.Add(off), hub.Day(base)
	return t, nil
}

// turnLines describe the rota for hub status and digest --dry-run.
func (a *app) turnLines(store hub.Store) []string {
	if a.cfg.Collector.DigestMode != "one" {
		return []string{"  Digest: each collector sends its own"}
	}
	now := a.now()
	t, err := a.turnAt(now)
	if err != nil {
		return nil
	}
	h, m, _ := config.ParseTime(a.cfg.Collector.DigestTime)
	at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location()).
		Add(time.Duration(t.rank) * a.cfg.Collector.TakeoverAfter.D())
	out := []string{
		fmt.Sprintf("  Digest: one per hub · this collector is #%d of %d (%s) · order %s",
			t.rank+1, len(t.order), at.Format("15:04"), strings.Join(t.order, ", ")),
	}
	if n, ok := store.LoadNotice(); ok && n.Date == hub.Day(now) {
		out = append(out, fmt.Sprintf("  Today: sent by %s at %s", n.By, n.At.In(now.Location()).Format("15:04")))
	} else {
		out = append(out, "  Today: no digest sent yet")
	}
	if len(a.cfg.Hub.Collectors) == 0 {
		out = append(out, "  ! hub.collectors is empty: no notice reaches the other collectors, so they send their own digest too")
	}
	return out
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
	sent    time.Time // slot of the last digest sent or handled
	lastTry time.Time
}

func newDigester(a *app, store hub.Store, logf func(string, ...any)) *digester {
	g := &digester{a: a, store: store, logf: logf, sent: store.LoadDigest().Slot}
	if g.sent.IsZero() {
		// A new collector waits for its next turn: a digest the minute it
		// is installed would compare with nothing and surprise.
		if t, err := a.turnAt(a.now()); err == nil {
			g.sent = t.slot
		}
	}
	return g
}

// tick sends the digest when it is due. config.Validate has checked the
// time, and a SIGHUP may have changed it, so the turn is worked out every
// tick. With digest_mode one, a digest some collector already sent today
// is not sent again, and one this collector sends is announced to the
// others.
func (g *digester) tick(ctx context.Context) {
	now := g.a.now()
	t, err := g.a.turnAt(now)
	if err != nil {
		return
	}
	if !g.sent.Before(t.base) && g.sent.Before(t.slot) {
		// This day's digest went out at an earlier slot of this collector:
		// the turn moved since (an upgrade from digest_mode each, a peer
		// added). Without this, the upgrade day would get a second one.
		g.sent = t.slot
		return
	}
	if !hub.DigestDue(now, t.slot, g.sent, g.lastTry) {
		return
	}
	g.lastTry = now
	d, next, err := g.a.buildDigest(ctx, g.store, &t)
	if errors.Is(err, hub.ErrNoDigestYet) {
		g.logf("digest skipped: %v", err)
		g.sent = t.slot
		return
	}
	if err != nil {
		g.logf("digest not built, trying again in %s: %v", hub.RetryEvery, err)
		return
	}
	if t.one {
		// Checked before every try, retries included: a collector whose
		// relay refused at its slot stays quiet once another took over.
		if n, ok := g.store.LoadNotice(); ok && n.Date == t.day {
			g.logf("digest skipped: sent by %s at %s", n.By, n.At.In(now.Location()).Format("15:04"))
			// Saved anyway, so that "new" in a digest this collector sends
			// later compares with yesterday's, not with its own last one.
			g.done(t.slot, next)
			return
		}
	}
	to, err := g.a.mailDigest(ctx, d)
	switch {
	case errors.Is(err, errNoDigestRecipients):
		g.logf("digest not sent: %v", err)
		g.sent = t.slot
		return
	case err != nil:
		g.logf("digest not sent, trying again in %s: %v", hub.RetryEvery, err)
		return
	default:
		g.logf("digest sent → %s (%s)", strings.Join(to, ", "), d.Tally())
	}
	g.done(t.slot, next)
	if t.one {
		g.tell(ctx, t)
	}
}

// done records the slot as handled.
func (g *digester) done(slot time.Time, next hub.DigestState) {
	g.sent = slot
	next.Slot = slot
	if err := g.store.SaveDigest(next); err != nil {
		g.logf("digest state not saved: %v", err)
	}
}

// tell announces the digest to every collector of the hub. A collector it
// misses sends its own digest at its turn: one mail too many, never none.
func (g *digester) tell(ctx context.Context, t turn) {
	cs := g.a.cfg.Hub.Collectors
	if len(cs) == 0 {
		g.logf("digest notice not sent: hub.collectors is empty, so the other collectors send their own digest")
		return
	}
	key, err := hub.LoadKey(g.a.cfg.Hub.KeyFile)
	if err != nil {
		g.logf("digest notice not sent: hub.key_file: %v", err)
		return
	}
	now := g.a.now()
	n := hub.Notice{Schema: 1, Date: t.day, By: g.a.label(), At: now}
	outs := hub.Tell(ctx, key, cs, n, now, hub.SendTimeout)
	for _, o := range outs {
		if o.Err != nil {
			g.logf("digest notice: collector %s not reached → %v", o.Collector, o.Err)
		}
	}
	g.logf("digest notice → %d of %d collector(s)", hub.Accepted(outs), len(outs))
}
