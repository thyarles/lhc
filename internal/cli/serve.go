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

	"github.com/thyarles/lhc-go/internal/config"
	"github.com/thyarles/lhc-go/internal/schedule"
)

func newServeCmd(a *app) *cobra.Command {
	var every, at string
	var now bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Stay running and run on the schedule (for hosts with neither systemd nor cron)",
		Long: `Run in the foreground and start a run at every scheduled time, with the
same random delay a timer would add. SIGHUP re-reads the config; SIGINT or
SIGTERM stop it. A failing run is logged and the loop carries on.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)

			override := func() error {
				if at != "" {
					a.cfg.Schedule.Time = at
				}
				if every != "" {
					d, err := config.ParseDuration(every)
					if err != nil {
						return err
					}
					a.cfg.Schedule.Every = config.Duration(d)
				}
				return a.cfg.Validate()
			}
			if err := a.load(); err != nil {
				return err
			}
			if err := override(); err != nil {
				return err
			}
			if now {
				a.serveOnce(ctx)
			}
			for {
				next, err := schedule.Next(a.cfg.Schedule, a.now())
				if err != nil {
					return err
				}
				// The jitter is applied here, before waking, exactly like a
				// systemd timer's RandomizedDelaySec.
				var delay time.Duration
				if a.cfg.Schedule.Random {
					w := schedule.Window(a.cfg.Schedule.RandomWindow, a.stderr)
					if e := a.cfg.Schedule.Every.D(); e > 0 && w > e {
						w = e - time.Minute
					}
					delay = schedule.Draw(w, nil)
				}
				wake := next.Add(delay)
				_, _ = fmt.Fprintf(a.stderr, "[%s] next run at %s (%s)\n",
					a.now().Format("2006-01-02 15:04:05"), wake.Format("2006-01-02 15:04"), schedule.Describe(a.cfg.Schedule))
				t := time.NewTimer(time.Until(wake))
				select {
				case <-ctx.Done():
					t.Stop()
					return nil
				case <-hup:
					t.Stop()
					old := a.cfg
					if err := a.load(); err == nil {
						err = override()
						if err != nil {
							a.cfg = old
						}
					}
					_, _ = fmt.Fprintf(a.stderr, "[%s] SIGHUP: config reloaded from %s\n", a.now().Format("2006-01-02 15:04:05"), a.configPath())
				case <-t.C:
					a.serveOnce(ctx)
				}
			}
		},
	}
	f := cmd.Flags()
	f.StringVar(&every, "every", "", "override schedule.every, e.g. 6h")
	f.StringVar(&at, "at", "", "override schedule.time, HH:MM")
	f.BoolVar(&now, "now", false, "also run once immediately")
	return cmd
}

// serveOnce runs one pass and never lets its failure end the loop.
func (a *app) serveOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintf(a.stderr, "lhc serve: run panicked: %v\n", r)
		}
	}()
	err := a.pass(ctx, runOpts{deliver: true, noRandom: true})
	if err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(a.stderr, "lhc serve: run failed: %v\n", err)
	}
}
