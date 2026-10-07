package cli

import (
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc-go/internal/runner"
)

func newPathsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "paths",
		Short: "Show where lhc reads its config and keeps state, reports and its log",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := a.load(); err != nil {
				return err
			}
			bin, _ := os.Executable()
			cfg := a.paths.Config
			if !a.cfgFound {
				cfg += "  (not found: defaults apply)"
			}
			a.printf("  binary  : %s", bin)
			a.printf("  config  : %s", cfg)
			a.printf("  state   : %s", a.paths.State)
			a.printf("  reports : %s", a.paths.Reports)
			a.printf("  log     : %s", a.paths.Log)
			a.printf("  PATH    : %s", strings.Join(runner.New().Path(), ":"))
			return nil
		},
	}
}
