package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Size is a byte count written as 1MiB, 512KiB, 2MB or a bare number of bytes.
type Size int64

var sizeText = regexp.MustCompile(`^(\d+)\s*([KMG]i?B|B)?$`)

var sizeUnit = map[string]int64{
	"": 1, "B": 1,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
	"KB": 1e3, "MB": 1e6, "GB": 1e9,
}

// ParseSize reads a Size.
func ParseSize(v string) (Size, error) {
	g := sizeText.FindStringSubmatch(strings.TrimSpace(v))
	if g == nil {
		return 0, fmt.Errorf("cannot read size %q: use 1MiB, 512KiB or a number of bytes", v)
	}
	n, err := strconv.ParseInt(g[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", v, err)
	}
	return Size(n * sizeUnit[g[2]]), nil
}

// String is the largest binary unit that divides the size exactly.
func (s Size) String() string {
	for _, u := range []string{"GiB", "MiB", "KiB"} {
		if d := sizeUnit[u]; s != 0 && int64(s)%d == 0 {
			return strconv.FormatInt(int64(s)/d, 10) + u
		}
	}
	return strconv.FormatInt(int64(s), 10)
}

func (s Size) MarshalYAML() (any, error) { return s.String(), nil }

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a size such as 1MiB", n.Line)
	}
	v, err := ParseSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*s = v
	return nil
}
