package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/fleet"
)

func newFleetCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Fleet key and the collector's view of the fleet",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newFleetKeygenCmd(a), newFleetStatusCmd(a), newFleetForgetCmd(a))
	return cmd
}

func newFleetKeygenCmd(a *app) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Write a new fleet key (once per fleet; copy the same file to every host)",
		Long: `Write 32 random bytes, base64, to fleet.key_file (or --out), mode 0600.
Every host and collector of the fleet needs this same file. An existing
file is never overwritten: a new key cuts off every host still holding the
old one.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if out == "" {
				if err := a.load(); err != nil {
					return err
				}
				out = a.cfg.Fleet.KeyFile
			}
			key, err := fleet.NewKey()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { //nolint:gosec // /etc/lhc is a normal config directory
				return err
			}
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; remove it first if you really mean to replace the fleet's key", out)
			}
			if err != nil {
				return err
			}
			if _, err := f.Write(key); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			a.printf("  ✓ Fleet key written to %s", out)
			a.printf("  Copy this same file to every host and collector, e.g. install.sh --fleet-key-file %s", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write (default: fleet.key_file)")
	return cmd
}

func newFleetStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List the hosts this collector has heard from, newest report per host",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			store := fleet.Store{Dir: filepath.Join(a.paths.State, "fleet")}
			entries, err := store.List()
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				a.printf("  No reports yet in %s", store.Dir)
				if !a.cfg.Collector.Enabled {
					a.printf("  This host is not a collector (collector.enabled is false).")
				}
				return nil
			}
			now := a.now()
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "HOST\tLAST REPORT\tOVERALL\tAGE")
			for _, e := range entries {
				if e.Err != nil {
					_, _ = fmt.Fprintf(w, "%s\t-\tunreadable: %v\t-\n", e.Host, e.Err)
					continue
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Host, e.ReceivedAt.Local().Format("2006-01-02 15:04"), e.Overall, age(now.Sub(e.ReceivedAt)))
			}
			return w.Flush()
		},
	}
}

func newFleetForgetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "forget HOST",
		Short: "Remove a decommissioned host from this collector",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if err := (fleet.Store{Dir: filepath.Join(a.paths.State, "fleet")}).Forget(args[0]); err != nil {
				return err
			}
			a.printf("  ✓ Forgot %s. If it reports again, it comes back.", args[0])
			return nil
		},
	}
}

// age reads like a person would say it: 45m, 3h, 2d 4h.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())-days*24)
	}
}
