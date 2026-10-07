// Package check holds the model every health check produces (Section, Row,
// Alert), the Check interface, and the registry that RunAll walks.
//
// This package never imports a check. Checks import it and register
// themselves from init(); internal/checks/all is the only place that imports
// every check, so there is no cycle and no central switch statement.
package check

import (
	"fmt"
	"strings"
)

// Status is ordered: a larger value is worse. Worse() and the report's
// worst-first ordering depend on that.
type Status int

const (
	OK Status = iota
	Info
	Caution
	Unhealthy
)

var statusNames = [...]string{"ok", "info", "caution", "unhealthy"}

func (s Status) String() string {
	if s < OK || s > Unhealthy {
		return "ok"
	}
	return statusNames[s]
}

// Word is the upper-case form used in reports and subject lines.
func (s Status) Word() string { return strings.ToUpper(s.String()) }

// Flagged reports whether a human is expected to look at it.
func (s Status) Flagged() bool { return s >= Caution }

// ParseStatus reads "ok", "info", "caution" or "unhealthy", any case.
func ParseStatus(v string) (Status, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	for i, n := range statusNames {
		if n == v {
			return Status(i), true
		}
	}
	return OK, false
}

// Worse returns the more severe of two statuses.
func Worse(a, b Status) Status {
	if a >= b {
		return a
	}
	return b
}

func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s *Status) UnmarshalText(b []byte) error {
	v, ok := ParseStatus(string(b))
	if !ok {
		return fmt.Errorf("unknown status %q", string(b))
	}
	*s = v
	return nil
}
