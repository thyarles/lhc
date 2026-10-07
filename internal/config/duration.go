package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var durToken = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*([dhms])`)

var durUnit = map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}

// ParseDuration reads 168h, 7d, 90m, 2h30m, "2h 30m", 45s, or a bare number
// of HOURS. Bare numbers mean hours because that is the unit schedules and
// reminders are discussed in; "4" meaning four seconds would be a trap.
// Anything with characters that do not belong to a token is rejected, so
// "4x" and "4h junk" are errors rather than four hours.
func ParseDuration(v string) (time.Duration, error) {
	text := strings.ToLower(strings.TrimSpace(v))
	if text == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("cannot read duration %q", v)
		}
		return time.Duration(math.Round(f * float64(time.Hour))), nil
	}
	matches := durToken.FindAllStringSubmatch(text, -1)
	var joined strings.Builder
	for _, m := range matches {
		joined.WriteString(m[0])
	}
	if len(matches) == 0 || strings.ReplaceAll(joined.String(), " ", "") != strings.ReplaceAll(text, " ", "") {
		return 0, fmt.Errorf("cannot read duration %q: use 4h, 90m, 2h30m or 7d", v)
	}
	var total float64
	for _, m := range matches {
		n, _ := strconv.ParseFloat(m[1], 64)
		total += n * float64(durUnit[m[2][0]])
	}
	return time.Duration(math.Round(total)), nil
}

// Duration is a time.Duration written in the ParseDuration grammar.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

// String is the shortest round-trippable form: 168h, 1h30m, 90s, 0.
func (d Duration) String() string {
	if d == 0 {
		return "0"
	}
	s := time.Duration(d).String() // e.g. 168h0m0s, 1h30m0s, 45s
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a duration such as 24h", n.Line)
	}
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }
