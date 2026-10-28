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

// A run below the broad alert said "daily list only" when the hub held the
// report and nothing was mailed at all.
func TestDestinationSaysWhereTheRunWent(t *testing.T) {
	for _, c := range []struct {
		due        bool
		heartbeat  string
		recipients int
		want       string
	}{
		{true, "auto", 1, "daily list only"},
		{true, "always", 1, "daily list only"},
		{false, "auto", 1, "left to the hub digest"},
		{false, "never", 1, "not mailed (email.heartbeat is never)"},
		{true, "auto", 0, "not mailed (no recipients)"},
	} {
		if got := destination(c.due, c.heartbeat, c.recipients); got != c.want {
			t.Errorf("destination(%v, %s, %d) = %q, want %q", c.due, c.heartbeat, c.recipients, got, c.want)
		}
	}
}
