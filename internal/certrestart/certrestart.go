// Package certrestart restarts rke2/k3s on a node before its certificates
// expire, as `lhc cert-restart` does: only when they are due, only when the
// service runs and the cluster is healthy, and only after a majority of the
// hub's collectors granted this node its turn in its group.
//
// Everything it does on the host goes through a runner.Runner, and the
// collectors through Deps.Ask, so every step is testable without a cluster.
package certrestart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/kube"
	"github.com/thyarles/lhc/internal/runner"
)

// Services are the units whose restart renews the certificates, in the
// order they are looked for.
var Services = []string{"rke2-server", "rke2-agent", "k3s", "k3s-agent"}

const (
	// restartTimeout bounds `systemctl restart`: rke2-server waits for etcd.
	restartTimeout = 5 * time.Minute
	// backTimeout is how long the service and the node get to come back.
	backTimeout = 10 * time.Minute
	pollEvery   = 15 * time.Second
)

// Options is what the config says.
type Options struct {
	Host       string
	Group      string
	Before     time.Duration
	Kubeconfig string // checks.kubernetes.kubeconfig; "" = the usual places
	// DryRun decides and asks the collectors with a probe: nothing is
	// recorded anywhere and nothing is restarted.
	DryRun bool
}

// Deps is the outside world.
type Deps struct {
	Runner runner.Runner
	Now    func() time.Time
	// Sleep waits d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Ask sends the claim to every collector; Collectors is how many there
	// are, for the majority.
	Ask        func(ctx context.Context, c hub.Claim) []hub.Reply
	Collectors int
	NewID      func() (string, error)
	Log        func(format string, args ...any)
}

// Outcome is how a run ended.
type Outcome string

const (
	NoCerts   Outcome = "no rke2/k3s certificates"
	NotDue    Outcome = "not due"
	NoService Outcome = "no active service"
	Unhealthy Outcome = "cluster not healthy"
	Refused   Outcome = "no majority"
	WouldDo   Outcome = "would restart"
	Restarted Outcome = "restarted"
)

// Result is what happened.
type Result struct {
	Outcome Outcome
	Service string
	At      time.Time // when the restart was started
	Renewed bool
	// Before and After are the first expiry before and after the restart.
	Before, After time.Time
}

func days(d time.Duration) int { return max(0, int(d/(24*time.Hour))) }

// Run does one night's decision and, when granted, the restart.
func Run(ctx context.Context, o Options, d Deps) (Result, error) {
	now := d.Now()
	certs := kube.ReadCerts(d.Runner)
	if !certs.Found() || len(certs.Leaves) == 0 {
		d.Log("no rke2/k3s certificates on this host: nothing to do")
		return Result{Outcome: NoCerts}, nil
	}
	first := certs.Leaves[0].NotAfter
	res := Result{Before: first}
	left := first.Sub(now)
	if left > o.Before {
		d.Log("not due: first certificate (%s) expires in %dd, restart at %dd", certs.Leaves[0].Base(), days(left), days(o.Before))
		res.Outcome = NotDue
		return res, nil
	}
	d.Log("due: first certificate (%s) expires in %dd (%s), restart at %dd", certs.Leaves[0].Base(), days(left), first.Format("2006-01-02"), days(o.Before))

	svc := ActiveService(ctx, d.Runner)
	if svc == "" {
		d.Log("none of %s is active: someone else's maintenance, trying again tomorrow", strings.Join(Services, ", "))
		res.Outcome = NoService
		return res, nil
	}
	res.Service = svc

	k := kube.Find(d.Runner, o.Kubeconfig)
	if k.Ready() {
		bad, err := k.NotReady(ctx)
		switch {
		case err != nil:
			d.Log("cannot tell whether the cluster is healthy (%v): trying again tomorrow", err)
			res.Outcome = Unhealthy
			return res, nil
		case len(bad) > 0:
			d.Log("not every node is Ready (%s): trying again tomorrow", strings.Join(bad, ", "))
			res.Outcome = Unhealthy
			return res, nil
		}
		d.Log("every node is Ready")
	} else {
		d.Log("no kubeconfig here (a worker): the collectors' SILENT rule guards the group")
	}

	id, err := d.NewID()
	if err != nil {
		return res, err
	}
	claim := hub.Claim{Schema: 1, Kind: hub.KindCertRestart, Host: o.Host, Group: o.Group, ID: id, Probe: o.DryRun}
	replies := d.Ask(ctx, claim)
	for _, r := range replies {
		d.Log("  %s: %s", r.Collector, r.Text())
	}
	need, got := hub.Majority(d.Collectors), hub.Grants(replies)
	if got < need {
		d.Log("%d of %d collectors granted, %d needed: trying again tomorrow", got, d.Collectors, need)
		res.Outcome = Refused
		return res, nil
	}
	if o.DryRun {
		d.Log("%d of %d collectors would grant: would restart %s (dry run, nothing done)", got, d.Collectors, svc)
		res.Outcome = WouldDo
		return res, nil
	}

	d.Log("%d of %d collectors granted (group %s): restarting %s", got, d.Collectors, o.Group, svc)
	res.At = d.Now()
	rctx, cancel := runner.WithTimeout(ctx, restartTimeout)
	out := d.Runner.Run(rctx, "systemctl", "restart", svc)
	cancel()
	if !out.OK() {
		msg := strings.TrimSpace(out.Stderr)
		if msg == "" && out.Err != nil {
			msg = out.Err.Error()
		}
		// Not a reason to stop: the service may still have come back.
		d.Log("systemctl restart %s failed: %s", svc, msg)
	}
	if err := waitBack(ctx, d, k, svc); err != nil {
		d.Log("%v", err)
	}

	res.Outcome = Restarted
	after := kube.ReadCerts(d.Runner)
	if len(after.Leaves) > 0 {
		res.After = after.Leaves[0].NotAfter
	}
	if !res.After.IsZero() && res.After.Sub(d.Now()) > o.Before {
		res.Renewed = true
		d.Log("renewed: first certificate now expires in %dd (%s)", days(res.After.Sub(d.Now())), res.After.Format("2006-01-02"))
	} else {
		d.Log("restart did not renew the certificates (first still expires in %dd)", days(res.After.Sub(d.Now())))
	}
	return res, nil
}

// ActiveService is the first of Services that systemd reports active, or "".
func ActiveService(ctx context.Context, r runner.Runner) string {
	for _, s := range Services {
		if strings.TrimSpace(r.Run(ctx, "systemctl", "is-active", s).Stdout) == "active" {
			return s
		}
	}
	return ""
}

// waitBack waits for the service to be active again and, with a
// kubeconfig, for every node to be Ready.
func waitBack(ctx context.Context, d Deps, k kube.Cluster, svc string) error {
	deadline := d.Now().Add(backTimeout)
	for {
		active := strings.TrimSpace(d.Runner.Run(ctx, "systemctl", "is-active", svc).Stdout) == "active"
		ready := true
		if active && k.Ready() {
			bad, err := k.NotReady(ctx)
			ready = err == nil && len(bad) == 0
		}
		if active && ready {
			if k.Ready() {
				d.Log("%s is active again and every node is Ready", svc)
			} else {
				d.Log("%s is active again", svc)
			}
			return nil
		}
		if !d.Now().Before(deadline) {
			if !active {
				return fmt.Errorf("%s is not active %s after the restart", svc, backTimeout)
			}
			return fmt.Errorf("%s is active, but not every node is Ready %s after the restart", svc, backTimeout)
		}
		if err := d.Sleep(ctx, pollEvery); err != nil {
			return errors.New("interrupted while waiting for " + svc)
		}
	}
}
