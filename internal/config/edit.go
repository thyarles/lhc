package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/thyarles/lhc-go/internal/state"
)

// Assignment is one KEY=VALUE from `lhc config set`.
type Assignment struct {
	Path  []string
	Value string
}

// ParseAssignment reads "smtp.host=mail.example.com".
func ParseAssignment(s string) (Assignment, error) {
	k, v, ok := strings.Cut(s, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return Assignment{}, fmt.Errorf("%q: expected key=value, e.g. schedule.time=06:30", s)
	}
	parts := strings.Split(k, ".")
	if slices.Contains(parts, "") {
		return Assignment{}, fmt.Errorf("%q: malformed key", k)
	}
	return Assignment{Path: parts, Value: strings.TrimSpace(v)}, nil
}

// Set applies assignments to the YAML in src and returns the new document.
//
// It edits the YAML node tree rather than re-rendering the struct, so the
// operator's comments and ordering survive. Every key is checked against the
// schema first: a typo stops the edit instead of becoming a line that does
// nothing. The result is parsed and validated before it is returned.
func Set(src []byte, assigns []Assignment) ([]byte, error) {
	schema, err := schemaTree()
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("config file is not a YAML mapping")
	}
	for _, a := range assigns {
		want := lookup(schema, a.Path)
		if want == nil {
			return nil, fmt.Errorf("%s: no such setting", strings.Join(a.Path, "."))
		}
		if want.Kind == yaml.MappingNode {
			return nil, fmt.Errorf("%s is a section; set one of its keys, e.g. %s.%s",
				strings.Join(a.Path, "."), strings.Join(a.Path, "."), want.Content[0].Value)
		}
		setPath(root, a.Path, valueNode(want, a.Value))
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	c, err := Parse(buf.Bytes())
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SetFile applies assignments to the file at path, creating it from the
// commented example when it does not exist. The previous version is kept as
// path.bak.
func SetFile(path string, assigns []Assignment) error {
	src, err := os.ReadFile(path)
	existed := err == nil
	if errors.Is(err, os.ErrNotExist) {
		src, err = Example, nil
	}
	if err != nil {
		return err
	}
	out, err := Set(src, assigns)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if existed {
		if err := os.WriteFile(path+".bak", src, 0o600); err != nil {
			return err
		}
	}
	return state.WriteFileAtomic(path, out, 0o600)
}

// schemaTree is the default config as a node tree: it says which keys exist
// and what kind of value each takes.
func schemaTree() (*yaml.Node, error) {
	b, err := Default().Marshal()
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
}

func lookup(n *yaml.Node, path []string) *yaml.Node {
	for _, key := range path {
		if n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				next = n.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// valueNode builds the replacement for a key, shaped like the schema says:
// a list from comma-separated text, a quoted string for string settings (so
// 00:07 or "yes" stay strings), a plain scalar otherwise.
func valueNode(want *yaml.Node, v string) *yaml.Node {
	if want.Kind == yaml.SequenceNode {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: item})
			}
		}
		return seq
	}
	if want.Tag == "!!str" {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

func setPath(n *yaml.Node, path []string, val *yaml.Node) {
	for i, key := range path {
		var next *yaml.Node
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == key {
				next = n.Content[j+1]
				if i == len(path)-1 {
					// Keep the comment that sat beside the old value.
					val.LineComment = next.LineComment
					n.Content[j+1] = val
					return
				}
				break
			}
		}
		if i == len(path)-1 {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
			return
		}
		if next == nil || next.Kind != yaml.MappingNode {
			fresh := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if next == nil {
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, fresh)
			} else {
				// `key: {}` or `key:` (null) — replace in place.
				*next = *fresh
				fresh = next
			}
			next = fresh
		}
		n = next
	}
}

// Setting is one flattened key and its rendered value.
type Setting struct {
	Key   string
	Value string
}

// Flatten lists every setting as dotted keys, sorted. Passwords are masked.
func (c *Config) Flatten() ([]Setting, error) {
	b, err := c.Marshal()
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []Setting
	var walk func(prefix string, n *yaml.Node)
	walk = func(prefix string, n *yaml.Node) {
		switch n.Kind {
		case yaml.MappingNode:
			if len(n.Content) == 0 {
				out = append(out, Setting{prefix, "{}"})
			}
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i].Value
				if prefix != "" {
					k = prefix + "." + k
				}
				walk(k, n.Content[i+1])
			}
		case yaml.SequenceNode:
			items := make([]string, 0, len(n.Content))
			for _, it := range n.Content {
				items = append(items, it.Value)
			}
			out = append(out, Setting{prefix, "[" + strings.Join(items, ", ") + "]"})
		default:
			v := n.Value
			if strings.HasSuffix(prefix, "password") && v != "" {
				v = "********"
			}
			out = append(out, Setting{prefix, v})
		}
	}
	walk("", doc.Content[0])
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Diff lists the settings of c that differ from the defaults.
func (c *Config) Diff() ([]Setting, error) {
	mine, err := c.Flatten()
	if err != nil {
		return nil, err
	}
	base, err := Default().Flatten()
	if err != nil {
		return nil, err
	}
	def := make(map[string]string, len(base))
	for _, s := range base {
		def[s.Key] = s.Value
	}
	var out []Setting
	for _, s := range mine {
		if d, ok := def[s.Key]; !ok || d != s.Value {
			out = append(out, s)
		}
	}
	return out, nil
}
