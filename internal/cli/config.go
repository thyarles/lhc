package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/config"
	"github.com/thyarles/lhc/internal/state"
)

func newConfigCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Create, inspect, change and validate the config file",
	}
	cmd.AddCommand(newConfigInitCmd(a), newConfigShowCmd(a), newConfigSetCmd(a), newConfigValidateCmd(a))
	return cmd
}

func newConfigInitCmd(a *app) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write the commented example config",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path := a.configPath()
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite, or `lhc config set` to change it)", path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			// 0600: the file will hold the SMTP password.
			if err := state.WriteFileAtomic(path, config.Example, 0o600); err != nil {
				return err
			}
			a.printf("  ✓ Created %s", path)
			a.printf("  Next: lhc config set email.daily_recipients=ops@example.com smtp.host=relay.example.com")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return cmd
}

func newConfigShowCmd(a *app) *cobra.Command {
	var diff bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print every effective setting (--diff: only what differs from the defaults)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := a.load(); err != nil {
				return err
			}
			list := a.cfg.Flatten
			if diff {
				list = a.cfg.Diff
			}
			settings, err := list()
			if err != nil {
				return err
			}
			src := a.paths.Config
			if !a.cfgFound {
				src += " (not found: defaults)"
			}
			a.printf("# %s", src)
			if diff && len(settings) == 0 {
				a.printf("# no overrides: this host runs on the defaults")
			}
			for _, s := range settings {
				a.printf("%s = %s", s.Key, s.Value)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&diff, "diff", false, "only settings that differ from the defaults")
	return cmd
}

func newConfigSetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY=VALUE...",
		Short: "Change settings, keeping the file's comments (lists are comma-separated)",
		Example: `  lhc config set schedule.time=06:30 schedule.random_window=2h
  lhc config set email.daily_recipients=ops@example.com,oncall@example.com
  lhc config set checks.disk.caution=85`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var assigns []config.Assignment
			for _, kv := range args {
				as, err := config.ParseAssignment(kv)
				if err != nil {
					return err
				}
				assigns = append(assigns, as)
			}
			path := a.configPath()
			if err := config.SetFile(path, assigns); err != nil {
				return err
			}
			for _, kv := range args {
				a.printf("  ✓ %s", kv)
			}
			a.printf("  Saved %s", path)
			return nil
		},
	}
}

func newConfigValidateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the config file strictly: unknown keys and bad values are errors",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path := a.configPath()
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s does not exist (create it with `lhc config init`)", path)
			}
			if err := a.load(); err != nil {
				return err
			}
			a.printf("  ✓ %s is valid", path)
			return nil
		},
	}
}
