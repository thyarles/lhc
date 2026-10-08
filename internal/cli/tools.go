package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/checks/tools"
	"github.com/thyarles/lhc/internal/host"
	"github.com/thyarles/lhc/internal/runner"
)

func newToolsCmd(a *app) *cobra.Command {
	var install, optional bool
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "List the system tools the checks use, and optionally install the missing ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := runner.New()
			pm := host.PkgManager(r.LookPath)
			osr := map[string]string{}
			if b, err := r.ReadFile("/etc/os-release"); err == nil {
				osr = host.ParseOSRelease(b)
			}
			a.printf("  OS       : %s", or(osr["PRETTY_NAME"], "unknown"))
			a.printf("  Packages : %s", or(pm, "(not detected)"))
			a.printf("")
			var missing []check.Tool
			for _, t := range tools.Tools {
				_, ok := r.LookPath(t.Name)
				tag := ""
				if t.Optional {
					tag = " (optional)"
				}
				switch {
				case ok:
					a.printf("  ✓ %-18s installed%s", t.Name, tag)
				case t.Optional && !optional:
					a.printf("  · %-18s not installed%s", t.Name, tag)
				default:
					a.printf("  ✗ %-18s missing%s → %s", t.Name, tag, t.InstallCmd(pm))
					missing = append(missing, t)
				}
			}
			if len(missing) == 0 {
				a.printf("\n  Nothing to install.")
				return nil
			}
			if !install {
				a.printf("\n  Install them with: lhc tools --install%s", map[bool]string{true: " --optional"}[optional])
				return nil
			}
			if pm == "" {
				return errors.New("no supported package manager (dnf, yum, apt-get, zypper) found")
			}
			if !isRoot() {
				return errors.New("installing packages needs root")
			}
			var failed []string
			for _, t := range missing {
				pkg := t.Package(pm)
				a.printf("  → %s", t.InstallCmd(pm))
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
				res := r.Run(ctx, pm, installArgs(pm, pkg)...)
				cancel()
				if !res.OK() {
					failed = append(failed, pkg)
					a.printf("    FAILED: %s", firstNonEmpty(res.Stderr, res.Stdout, fmt.Sprint(res.Err)))
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("could not install: %s", strings.Join(failed, ", "))
			}
			a.printf("  ✓ Done")
			return nil
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "install the missing tools (root)")
	cmd.Flags().BoolVar(&optional, "optional", false, "include optional tools (docker, kubectl, rkhunter, ...)")
	return cmd
}

func installArgs(pm, pkg string) []string {
	if pm == "zypper" {
		return []string{"--non-interactive", "install", pkg}
	}
	return []string{"install", "-y", pkg}
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			l, _, _ := strings.Cut(v, "\n")
			return l
		}
	}
	return ""
}
