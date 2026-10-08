package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/version"

	// Every check registers itself.
	_ "github.com/thyarles/lhc/internal/checks/all"
)

// exitError carries a specific exit status out of a command.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// Execute runs the command line and returns the process exit status.
func Execute() int {
	return run(os.Args[1:], os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	a := newApp(stdout, stderr)
	root := newRoot(a)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "lhc:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		return 1
	}
	return 0
}

func newRoot(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "lhc",
		Short: "Linux Health Check: a daily snapshot of a host, mailed to the people who care",
		Long: `lhc inspects a Linux host (disk, memory, services, ports, packages,
logins, Kubernetes, ...), remembers what it saw, and mails a report:
a short heartbeat to the daily list every run, and a separate alert to
the broad list only when something NEW needs attention.`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().StringVar(&a.configFlag, "config", "", "config file (default: $LHC_CONFIG, /etc/lhc/config.yaml as root, ~/.config/lhc/config.yaml)")
	root.PersistentFlags().BoolVarP(&a.verbose, "verbose", "v", false, "debug logging")
	root.AddCommand(
		newRunCmd(a), newReportCmd(a), newConfigCmd(a), newInstallCmd(a), newUninstallCmd(a),
		newServeCmd(a), newToolsCmd(a), newPathsCmd(a), newVersionCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
