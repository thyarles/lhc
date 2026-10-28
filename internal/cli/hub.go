package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/hub"
	"github.com/thyarles/lhc/internal/report"
)

func newHubCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hub",
		Short: "Hub key and what a collector has received",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newHubKeygenCmd(a), newHubStatusCmd(a), newHubForgetCmd(a), newHubDigestCmd(a))
	return cmd
}

func newHubKeygenCmd(a *app) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Write a new hub key (once per hub; copy the same file to every host)",
		Long: `Write 32 random bytes, base64, to hub.key_file (or --out), mode 0600.
Every host and collector of the hub needs this same file. An existing
file is never overwritten: a new key cuts off every host still holding the
old one.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if out == "" {
				if err := a.load(); err != nil {
					return err
				}
				out = a.cfg.Hub.KeyFile
			}
			key, err := hub.NewKey()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { //nolint:gosec // /etc/lhc is a normal config directory
				return err
			}
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; remove it first if you really mean to replace the hub key", out)
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
			a.printf("  ✓ Hub key written to %s", out)
			a.printf("  Copy this same file to every host and collector, e.g. install.sh --hub-key-file %s", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write (default: hub.key_file)")
	return cmd
}

func newHubStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List the hosts this collector has heard from, newest report per host",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			store := a.hubStore()
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
			if a.cfg.Collector.Enabled {
				for _, l := range a.turnLines(store) {
					a.printf("%s", l)
				}
				for _, e := range entries {
					switch {
					case e.Host != a.label() || !e.RelayChecked:
					case e.RelayErr != "":
						a.printf("  Relay: unreachable at %s → %s", e.Generated.In(now.Location()).Format("15:04"), e.RelayErr)
					default:
						a.printf("  Relay: reachable (checked %s)", e.Generated.In(now.Location()).Format("15:04"))
					}
				}
				a.printf("")
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "HOST\tLAST REPORT\tOVERALL\tVERSION\tAGE")
			for _, e := range entries {
				if e.Err != nil {
					_, _ = fmt.Fprintf(w, "%s\t-\tunreadable: %v\t-\t-\n", e.Host, e.Err)
					continue
				}
				v := e.Version
				if v == "" {
					v = "-"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Host, e.ReceivedAt.Local().Format("2006-01-02 15:04"), e.Overall, v, report.Ago(now.Sub(e.ReceivedAt)))
			}
			return w.Flush()
		},
	}
}

func newHubForgetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "forget HOST",
		Short: "Remove a decommissioned host from this collector",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.load(); err != nil {
				return err
			}
			if err := a.hubStore().Forget(args[0]); err != nil {
				return err
			}
			a.printf("  ✓ Forgot %s. If it reports again, it comes back.", args[0])
			return nil
		},
	}
}
