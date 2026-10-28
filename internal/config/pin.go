package config

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A config file holds only what someone chose. Every other setting stays in
// it as a commented-out line, `#key: value`, showing the installed version's
// default: the file still documents itself, and a new default reaches the
// host on upgrade. Files written before this pinned every default of the
// day they were created (#46).

// pastDefaults are values a released lhc shipped as the default of a key
// whose default has changed since. A file holding one of them was written
// by an older install, not by someone choosing it. Add an entry whenever a
// default changes.
var pastDefaults = map[string][]string{
	"checks.processes.zombie_caution":   {"10"}, // before v0.9.0 (#38)
	"checks.processes.zombie_unhealthy": {"50"}, // before v0.9.0 (#38)
}

// A commented-out setting: "#" right before a key or a list item. Prose
// comments have a space after the "#".
var settingLine = regexp.MustCompile(`^(\s*)#((?:[A-Za-z_][A-Za-z0-9_-]*:|-)(?:\s.*)?)$`)

// view is a file with every commented-out setting active again. Lines keep
// their numbers, so a node's Line indexes both.
type view struct {
	orig, full []string
	was        []bool // line i (0-based) is a commented-out setting
	root       *yaml.Node
}

// newView parses src as if nothing were commented out. When that does not
// parse (a hand-written comment that only looks like a setting), only the
// active lines count.
func newView(src []byte) (*view, error) {
	v := &view{orig: strings.Split(string(src), "\n")}
	v.full = make([]string, len(v.orig))
	v.was = make([]bool, len(v.orig))
	for i, l := range v.orig {
		if m := settingLine.FindStringSubmatch(l); m != nil {
			v.full[i], v.was[i] = m[1]+m[2], true
		} else {
			v.full[i] = l
		}
	}
	root, err := mappingRoot([]byte(strings.Join(v.full, "\n")))
	if err != nil {
		copy(v.full, v.orig)
		clear(v.was)
		if root, err = mappingRoot(src); err != nil {
			return nil, err
		}
	}
	v.root = root
	return v, nil
}

// mappingRoot is the top mapping of a document; nil for an empty one.
func mappingRoot(b []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		return nil, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config file is not a YAML mapping")
	}
	return doc.Content[0], nil
}

// entry is one key of the file: a setting (scalar, list, or a one-line
// flow mapping) or a section heading.
type entry struct {
	path        []string
	key, val    *yaml.Node
	first, last int // 0-based line range
	section     bool
	children    []*entry
}

func (e *entry) name() string { return strings.Join(e.path, ".") }

// entries lists the keys under m, depth first.
func entries(m *yaml.Node, prefix []string) []*entry {
	if m == nil {
		return nil
	}
	var out []*entry
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, val := m.Content[i], m.Content[i+1]
		e := &entry{path: append(slices.Clone(prefix), k.Value), key: k, val: val, first: k.Line - 1, last: lastLine(val) - 1}
		if val.Kind == yaml.MappingNode && val.Style&yaml.FlowStyle == 0 && len(val.Content) > 0 {
			e.section = true
			e.children = entries(val, e.path)
		}
		out = append(out, e)
	}
	return out
}

func lastLine(n *yaml.Node) int {
	l := n.Line
	for _, c := range n.Content {
		l = max(l, lastLine(c))
	}
	return l
}

// editable says whether a setting can be rewritten as one line: not a
// block scalar, whose last line the node tree does not record.
func editable(e *entry) bool {
	return e.val.Kind != yaml.ScalarNode || e.val.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0
}

// find is the entry at path and the sections above it that exist. blocked
// is a key on the way that is not a block section (`key: {}`, a flow
// mapping, a null): the text editor cannot add under it.
func find(es []*entry, path []string) (hit *entry, chain []*entry, blocked bool) {
	for i, seg := range path {
		hit = nil
		for _, e := range es {
			if e.path[len(e.path)-1] == seg {
				hit = e
				break
			}
		}
		switch {
		case hit == nil:
			return nil, chain, false
		case i == len(path)-1:
			return hit, chain, false
		case !hit.section:
			return nil, chain, true
		}
		chain = append(chain, hit)
		es = hit.children
	}
	return nil, chain, false
}

func indentOf(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }

// commentOut puts "#" right after the indentation.
func commentOut(l string) string {
	if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
		return l
	}
	in := indentOf(l)
	return in + "#" + l[len(in):]
}

// render is a value as it is written after "key: ".
func render(n *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if n.Kind == yaml.SequenceNode {
		c := *n
		c.Style |= yaml.FlowStyle
		n = &c
	}
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// settingText is a one-line setting with the comment that sat beside it.
func settingText(indent, key, value string, e *entry) string {
	s := indent + key + ": " + value
	if e != nil {
		c := e.val.LineComment
		if c == "" {
			c = e.key.LineComment
		}
		if c != "" {
			s += "  " + c
		}
	}
	return s
}

// Unpinned is a setting Unpin commented out.
type Unpinned struct {
	Key, Value, Default string
	Past                bool // an earlier release's default, not today's
}

// Unpin comments out every setting of src that holds a shipped default,
// current or past, so the running binary's default applies, and rewrites
// every commented-out setting to show today's default. Anything else is
// someone's choice and stays. It returns src unchanged when nothing moved.
func Unpin(src []byte) ([]byte, []Unpinned, error) {
	c, err := Parse(src)
	if err != nil {
		return nil, nil, err
	}
	eff, err := flatMap(c)
	if err != nil {
		return nil, nil, err
	}
	def, err := flatMap(Default())
	if err != nil {
		return nil, nil, err
	}
	schema, err := schemaTree()
	if err != nil {
		return nil, nil, err
	}
	v, err := newView(src)
	if err != nil {
		return nil, nil, err
	}
	// What the commented-out lines say, to spot one showing an old default.
	shown := map[string]string{}
	if fc, err := Parse([]byte(strings.Join(v.full, "\n"))); err == nil {
		if m, err := flatMap(fc); err == nil {
			shown = m
		}
	}

	out := slices.Clone(v.orig)
	var done []Unpinned
	// walk returns whether every setting under es ends up commented out.
	var walk func(es []*entry) bool
	walk = func(es []*entry) bool {
		all := true
		for _, e := range es {
			if e.section {
				if walk(e.children) {
					out[e.first] = commentOut(v.full[e.first])
				} else {
					out[e.first], all = v.full[e.first], false
				}
				continue
			}
			key := e.name()
			wasOff := v.was[e.first]
			isDef := defaultHere(key, eff, def)
			if !wasOff && (!isDef || !editable(e)) {
				all = false
				continue
			}
			if !wasOff {
				u := Unpinned{Key: key, Value: eff[key], Default: def[key]}
				for _, p := range pastDefaults[key] {
					u.Past = u.Past || eff[key] == p && p != def[key]
				}
				done = append(done, u)
			}
			// Show today's default on the commented line. A line that already
			// does keeps its own spacing.
			now := eff[key]
			if wasOff {
				now = shown[key]
			}
			stale := now != def[key]
			if want := lookup(schema, e.path); stale && want != nil && want.Kind != yaml.MappingNode && e.first == e.last && editable(e) {
				if r, err := render(want); err == nil {
					out[e.first] = commentOut(settingText(indentOf(v.full[e.first]), e.path[len(e.path)-1], r, e))
					continue
				}
			}
			for i := e.first; i <= e.last; i++ {
				out[i] = commentOut(v.full[i])
			}
		}
		return all
	}
	walk(entries(v.root, nil))

	res := []byte(strings.Join(out, "\n"))
	if bytes.Equal(res, src) {
		return src, nil, nil
	}
	// The effective config must not move, except the past defaults that now
	// follow today's. Anything else is a bug here: keep the file.
	after, err := Parse(res)
	if err == nil {
		err = after.Validate()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("unpinning the defaults would break the file: %w", err)
	}
	got, err := flatMap(after)
	if err != nil {
		return nil, nil, err
	}
	for k, want := range eff {
		if slices.Contains(pastDefaults[k], want) {
			want = def[k]
		}
		if got[k] != want {
			return nil, nil, fmt.Errorf("unpinning the defaults would change %s from %s to %s", k, want, got[k])
		}
	}
	return res, done, nil
}

// defaultHere says whether key holds a shipped default. A one-line flow
// mapping counts when every setting in it does.
func defaultHere(key string, eff, def map[string]string) bool {
	if d, ok := def[key]; ok {
		return eff[key] == d || slices.Contains(pastDefaults[key], eff[key])
	}
	found := false
	for k, d := range def {
		if strings.HasPrefix(k, key+".") {
			found = true
			if eff[k] != d && !slices.Contains(pastDefaults[k], eff[k]) {
				return false
			}
		}
	}
	return found
}

func flatMap(c *Config) (map[string]string, error) {
	flat, err := c.Flatten()
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(flat))
	for _, s := range flat {
		m[s.Key] = s.Value
	}
	return m, nil
}

// CommentedExample is the example with every setting commented out: what
// `lhc config init` and a first `lhc config set` write.
func CommentedExample() []byte {
	out, _, err := Unpin(Example)
	if err != nil {
		panic("the embedded example cannot be commented out: " + err.Error())
	}
	return out
}

// setLine applies one assignment as a text edit: the setting's line is
// replaced, or added under its section, and the sections above it are made
// active. Everything else in the file stays byte for byte. It returns false
// when the file's shape needs the node editor (flow-style sections).
func setLine(src []byte, path []string, val *yaml.Node) ([]byte, bool) {
	v, err := newView(src)
	if err != nil {
		return nil, false
	}
	value, err := render(val)
	if err != nil {
		return nil, false
	}
	out := slices.Clone(v.orig)
	leaf, chain, blocked := find(entries(v.root, nil), path)
	if blocked {
		return nil, false
	}
	for _, s := range chain {
		out[s.first] = v.full[s.first]
	}
	key := path[len(path)-1]
	if leaf != nil {
		if leaf.section || !editable(leaf) {
			return nil, false
		}
		repl := settingText(indentOf(v.full[leaf.first]), key, value, leaf)
		out = slices.Replace(out, leaf.first, leaf.last+1, repl)
		return []byte(strings.Join(out, "\n")), true
	}

	// New key: under the deepest section present, after its last line, at
	// its children's indentation.
	at, indent, step := len(out), "", "  "
	if len(chain) > 0 {
		s := chain[len(chain)-1]
		at, indent = s.last+1, indentOf(v.full[s.first])
		if len(s.children) > 0 {
			step = strings.TrimPrefix(indentOf(v.full[s.children[0].first]), indent)
		}
		indent += step
	} else {
		// Trailing blank lines stay at the end of the file.
		for at > 0 && strings.TrimSpace(out[at-1]) == "" {
			at--
		}
	}
	var add []string
	for _, seg := range path[len(chain) : len(path)-1] {
		add = append(add, indent+seg+":")
		indent += step
	}
	add = append(add, settingText(indent, key, value, nil))
	out = slices.Insert(out, at, add...)
	return []byte(strings.Join(out, "\n")), true
}
