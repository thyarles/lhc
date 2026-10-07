package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc-go/internal/config"
	"github.com/thyarles/lhc-go/internal/runner"
	"github.com/thyarles/lhc-go/internal/schedule"
)

func newInstallCmd(a *app) *cobra.Command {
	var at, every, backend string
	var noRandom bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Schedule lhc: a systemd timer where possible, otherwise a crontab entry",
		Long: `Write the config (if there is none), record any schedule flags in it, and
install the schedule. Re-running it is safe: it replaces what it installed.

systemd is used when running as root on systemd 229 or later (timers with a
random delay); otherwise cron. With neither, run "lhc serve" under your own
supervisor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := a.configPath()
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err := config.SetFile(path, nil); err != nil {
					return err
				}
				a.printf("  ✓ Created %s from the example", path)
			}
			var sets []string
			if at != "" {
				sets = append(sets, "schedule.time="+at)
			}
			if cmd.Flags().Changed("every") {
				sets = append(sets, "schedule.every="+every)
			}
			if noRandom {
				sets = append(sets, "schedule.random=false")
			}
			if len(sets) > 0 {
				var assigns []config.Assignment
				for _, s := range sets {
					as, err := config.ParseAssignment(s)
					if err != nil {
						return err
					}
					assigns = append(assigns, as)
				}
				// Persisted, so the next install or upgrade keeps this host's time.
				if err := config.SetFile(path, assigns); err != nil {
					return err
				}
			}
			if err := a.load(); err != nil {
				return err
			}
			b := schedule.Backend(backend)
			switch b {
			case "":
				b = schedule.Detect(cmd.Context(), a.runner, isRoot())
			case schedule.Systemd, schedule.Cron:
			default:
				return fmt.Errorf("unknown --backend %q (systemd or cron)", backend)
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			if real, err := filepath.EvalSymlinks(bin); err == nil {
				bin = real
			}
			in := &schedule.Installer{Runner: a.runner, Out: a.printf}
			if err := in.Install(cmd.Context(), b, a.cfg.Schedule, schedule.Job{Binary: bin, Config: path, Log: a.paths.Log}); err != nil {
				return err
			}
			a.printf("  Schedule: %s", schedule.Describe(a.cfg.Schedule))
			if len(a.cfg.Email.DailyRecipients) == 0 {
				a.printf("  ! No recipients yet: lhc config set email.daily_recipients=ops@example.com smtp.host=relay.example.com")
			}
			a.printf("  Preview a report now with: lhc report")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&at, "time", "", "first run of the day, HH:MM (saved as schedule.time)")
	f.StringVar(&every, "every", "", "run every N hours, e.g. 6h; 0 = daily (saved as schedule.every)")
	f.BoolVar(&noRandom, "no-random", false, "no random delay (saved as schedule.random=false)")
	f.StringVar(&backend, "backend", "", "force systemd or cron")
	return cmd
}

func newUninstallCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the systemd timer or crontab entry (config, state and reports are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A broken config must not stop anyone removing the schedule, so
			// it is not loaded at all.
			in := &schedule.Installer{Runner: runner.New(), Out: a.printf}
			if err := in.Uninstall(cmd.Context()); err != nil {
				return err
			}
			a.printf("  Kept: %s and the state directory", a.configPath())
			return nil
		},
	}
}
