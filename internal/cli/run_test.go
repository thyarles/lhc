package cli

import "testing"

func TestMailDue(t *testing.T) {
	for _, c := range []struct {
		alert                bool
		mode                 string
		collectors, accepted int
		want                 bool
	}{
		{true, "always", 0, 0, true},
		{true, "auto", 0, 0, true},
		{true, "auto", 2, 2, true},
		{true, "never", 0, 0, true},
		{true, "never", 2, 2, true},
		{false, "always", 0, 0, true},
		{false, "always", 2, 2, true},
		{false, "auto", 0, 0, true},
		{false, "auto", 2, 0, true},
		{false, "auto", 2, 1, false},
		{false, "auto", 2, 2, false},
		{false, "never", 0, 0, false},
		{false, "never", 2, 0, false},
	} {
		if got := mailDue(c.alert, c.mode, c.collectors, c.accepted); got != c.want {
			t.Errorf("mailDue(alert=%v, %s, %d collectors, %d accepted) = %v, want %v",
				c.alert, c.mode, c.collectors, c.accepted, got, c.want)
		}
	}
}
