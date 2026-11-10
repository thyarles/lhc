package cli

import (
	"testing"
	"time"

	"github.com/thyarles/lhc/internal/config"
)

func TestAnUpgradeRestartsAnEnabledCollector(t *testing.T) {
	// install.sh re-runs `lhc install` without --collector on an upgrade.
	// Skipping the collector then left it running the replaced binary.
	for _, c := range []struct {
		flag, enabled bool
		want          string
	}{
		{true, false, "--collector"},
		{true, true, "--collector"},
		{false, true, "collector.enabled"},
		{false, false, ""},
	} {
		if got := collectorReason(c.flag, c.enabled); got != c.want {
			t.Errorf("flag %v, enabled %v: %q, want %q", c.flag, c.enabled, got, c.want)
		}
	}
}

func TestTheCertRestartLineSaysDays(t *testing.T) {
	cr := config.CertRestart{Before: config.Duration(89 * 24 * time.Hour), Time: "03:00", Window: config.Duration(2 * time.Hour)}
	if got, want := certRestartLine(cr), "  Certificate restarts: nightly at 03:00 within 2h, when the first certificate expires within 89 days"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
