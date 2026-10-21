package cli

import "testing"

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
