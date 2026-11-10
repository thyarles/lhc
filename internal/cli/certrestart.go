package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/certrestart"
	"github.com/thyarles/lhc/internal/checks/kubernetes"
	"github.com/thyarles/lhc/internal/config"
	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/kube"
	"github.com/thyarles/lhc/internal/report"
	"github.com/thyarles/lhc/internal/state"
)

// certRestartOwner and certRestartKey hold the last restart lhc made, for
// the reports that follow it.
const (
	certRestartOwner = "cert-restart"
	certRestartKey   = "last"
)

// lastRestart is the state lhc cert-restart leaves for the reports.
type lastRestart struct {
	At      time.Time `json:"at"`
	Service string    `json:"service"`
	Renewed bool      `json:"renewed"`
}

func newCertRestartCmd(a *app) *cobra.Command {
	var scheduled, dryRun bool
	cmd := &cobra.Command{
		Use:   "cert-restart",
		Short: "Restart rke2/k3s before its certificates expire, one node of a group a night",
		Long: `Restart rke2-server, rke2-agent, k3s or k3s-agent when the node's first
certificate expires within cert_restart.before, so the service renews its
certificates on start.

It does nothing unless the certificates are due, the service is active and,
on a node with a kubeconfig, every node of the cluster is Ready. Then it asks
every collector in hub.collectors and restarts only when a majority grant
it: a collector grants one node of a group a day. After the restart it waits
for the service (and the nodes) to come back, logs whether the certificates
were renewed, and runs a normal lhc run so the collectors hear about it.

The lhc-cert-restart timer runs it with --scheduled every night.
--dry-run asks the collectors what they would answer and changes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.certRestart(ctx, dryRun)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&scheduled, "scheduled", false, "started by the lhc-cert-restart timer")
	f.BoolVar(&dryRun, "dry-run", false, "decide and ask the collectors, but record and restart nothing")
	return cmd
}

func (a *app) certRestart(ctx context.Context, dryRun bool) error {
	if err := a.load(); err != nil {
		return err
	}
	cr := a.cfg.CertRestart
	if cr.Before.D() == 0 {
		return errors.New("cert_restart.before is 0: certificate restarts are off (lhc config set cert_restart.before=30d)")
	}
	key, err := hub.LoadKey(a.cfg.Hub.KeyFile)
	if err != nil {
		return fmt.Errorf("hub.key_file: %w", err)
	}
	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(a.stderr, "[%s] %s\n", a.now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
	host := a.label()
	o := certrestart.Options{
		Host: host, Group: config.GroupOf(cr.Group, host), Before: cr.Before.D(),
		Kubeconfig: a.kubeconfigSetting(), DryRun: dryRun,
	}
	collectors := a.cfg.Hub.Collectors
	d := certrestart.Deps{
		Runner: a.runner,
		Now:    a.now,
		Sleep:  sleepCtx,
		Ask: func(ctx context.Context, c hub.Claim) []hub.Reply {
			return hub.Ask(ctx, key, collectors, c, a.now, hub.SendTimeout)
		},
		Collectors: len(collectors),
		NewID:      hub.NewClaimID,
		Log:        logf,
	}
	logf("cert-restart on %s, group %s", host, o.Group)
	res, err := certrestart.Run(ctx, o, d)
	if err != nil || res.Outcome != certrestart.Restarted {
		return err
	}
	st := &state.Dir{Path: a.paths.State, Now: a.now}
	if err := st.For(certRestartOwner).Save(certRestartKey, lastRestart{At: res.At, Service: res.Service, Renewed: res.Renewed}); err != nil {
		logf("saving the restart in the state: %v", err)
	}
	logf("running lhc run, so the collectors hear about it")
	return a.pass(ctx, runOpts{deliver: true, noRandom: true})
}

// kubeconfigSetting is checks.kubernetes.kubeconfig.
func (a *app) kubeconfigSetting() string {
	if c, ok := a.cfg.Check("kubernetes").(*kubernetes.Config); ok {
		return c.Kubeconfig
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// certRestartInfo is what a report tells the collectors about this host's
// cert_restart: its group, expiry and last restart. Nil when it is off.
func (a *app) certRestartInfo(ctx context.Context, states *state.Dir) *report.CertRestart {
	cr := a.cfg.CertRestart
	if cr.Before.D() == 0 {
		return nil
	}
	info := &report.CertRestart{
		Group:      config.GroupOf(cr.Group, a.label()),
		BeforeDays: int(cr.Before.D() / (24 * time.Hour)),
	}
	if c := kube.ReadCerts(a.runner); len(c.Leaves) > 0 {
		info.FirstExpiry = c.Leaves[0].NotAfter
		info.Service = certrestart.ActiveService(ctx, a.runner)
	}
	var last lastRestart
	if states.For(certRestartOwner).Load(certRestartKey, &last) {
		info.LastRestart, info.LastRenewed = last.At, last.Renewed
		if info.Service == "" {
			info.Service = last.Service
		}
	}
	return info
}
