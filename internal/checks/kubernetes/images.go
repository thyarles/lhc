package kubernetes

import (
	"context"
	"fmt"
	"strings"

	"github.com/thyarles/lhc/internal/check"
)

func imageRows(ctx context.Context, s *check.Section, env *check.Env, cfg *Config) {
	crictl := crictlBin(env.Runner)
	if crictl == "" {
		return // a node without crictl is not a defect: say nothing
	}
	var args []string
	if sock := criSocket(env.Runner); sock != "" {
		args = append(args, "--runtime-endpoint", "unix://"+sock)
	}
	args = append(args, "images")

	ctx, cancel := context.WithTimeout(ctx, crictlTimeout)
	defer cancel()
	res := env.Runner.Run(ctx, crictl, args...)
	if !res.OK() || res.Stdout == "" {
		return
	}
	lines := strings.Split(res.Stdout, "\n")[1:] // drop the IMAGE TAG IMAGE ID SIZE header
	if len(lines) == 0 {
		return
	}
	s.Add("Container Images", fmt.Sprintf("%d image(s) via crictl", len(lines)), check.Info)

	// Images churn on every deploy, so the listing is a wall and a diff would
	// be daily noise. Inventory is never CAUTION.
	if !cfg.ListImages {
		return
	}
	for _, line := range lines[:min(len(lines), listCap)] {
		if p := strings.Fields(line); len(p) >= 4 {
			s.Add("  "+p[0], fmt.Sprintf("%s  (%s)", p[1], p[3]), check.Info)
		}
	}
	if len(lines) > listCap {
		s.Add("…", fmt.Sprintf("and %d more image(s)", len(lines)-listCap), check.Info)
	}
}
