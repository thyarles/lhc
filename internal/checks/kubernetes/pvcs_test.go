package kubernetes

import (
	"strings"
	"testing"

	"github.com/thyarles/lhc-go/internal/check"
	"github.com/thyarles/lhc-go/internal/checktest"
)

func pvcs(t *testing.T, e *checktest.Env, lines ...string) *check.Section {
	t.Helper()
	cluster(e, oneNode)
	e.Fake.Expect(pvcCmd, strings.Join(lines, "\n"))
	return e.Run(Check{})
}

func TestAPVCPendingByDesignIsNotFlaggedOnFirstSight(t *testing.T) {
	// local-path and most cloud classes are WaitForFirstConsumer, so a claim
	// with no consumer is Pending forever ON PURPOSE.
	s := pvcs(t, newEnv(t), "default tmp-cache Pending local-path")
	if got := statusOf(t, s, "default/tmp-cache"); got != check.Info {
		t.Fatalf("status %v", got)
	}
	checktest.NoAlerts(t, s)
}

func TestAPVCPendingOnTwoRunsAlerts(t *testing.T) {
	e := newEnv(t)
	e.Seed(t, "pending_pvcs", []string{"default/tmp-cache"})
	s := pvcs(t, e, "default tmp-cache Pending local-path")
	if got := statusOf(t, s, "default/tmp-cache"); got != check.Caution {
		t.Fatalf("status %v", got)
	}
	alertCount(t, s, 1)
}

func TestALostPVCIsUnhealthyImmediately(t *testing.T) {
	s := pvcs(t, newEnv(t), "default data-postgres-0 Lost local-path")
	if s.Status != check.Unhealthy {
		t.Fatalf("status %v", s.Status)
	}
}

func TestBoundPVCsAreACount(t *testing.T) {
	s := pvcs(t, newEnv(t), "default data-postgres-0 Bound local-path", "monitoring prom-data Bound <none>")
	if got := statusOf(t, s, "PersistentVolumeClaims"); got != check.Info {
		t.Fatalf("status %v", got)
	}
	if v := checktest.Rows(s)["PersistentVolumeClaims"].Value; v != "2 bound" {
		t.Fatalf("value %q", v)
	}
	checktest.NoAlerts(t, s)
}

func TestNodeScopeSkipsPVCs(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Scope = "node" })
	cluster(e, oneNode)
	e.Run(Check{})
	if e.Fake.Ran(pvcCmd) {
		t.Fatal("PVCs queried with scope: node")
	}
}
