package check

import "testing"

func TestAFactNeverChangesTheSectionStatusOrRaisesAnAlert(t *testing.T) {
	s := NewSection("updates", "Pending Updates")
	s.Add("Pending Updates", "0", OK)
	s.Fact("reboot_required", "yes", Unhealthy)
	if s.Status != OK || len(s.Alerts) != 0 || len(s.Rows) != 1 {
		t.Fatalf("status %v, alerts %v, rows %d", s.Status, s.Alerts, len(s.Rows))
	}
	if len(s.Facts) != 1 || s.Facts[0] != (Fact{Key: "reboot_required", Value: "yes", Status: Unhealthy}) {
		t.Fatalf("facts %+v", s.Facts)
	}
}
