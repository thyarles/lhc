package kubernetes

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thyarles/lhc/internal/checktest"
)

const crictlHeader = "IMAGE                    TAG       IMAGE ID        SIZE"

func crictlImages(n int) string {
	lines := []string{crictlHeader}
	for i := range n {
		lines = append(lines, fmt.Sprintf("docker.io/library/app%d   v%d   sha%d   %dMB", i, i, i, i+1))
	}
	return strings.Join(lines, "\n")
}

func TestImagesAreACountByDefault(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		list     bool
		wantRows []string // labels expected beyond the cluster rows
	}{
		{"no images means no row", "", false, nil},
		{"header only means no row", crictlHeader, false, nil},
		{"a count, not a listing", crictlImages(3), false, []string{"Container Images"}},
		{"list_images lists them", crictlImages(2), true, []string{"Container Images", "  docker.io/library/app0", "  docker.io/library/app1"}},
		{"the listing is capped", crictlImages(12), true, append([]string{"Container Images"}, func() []string {
			var l []string
			for i := range 10 {
				l = append(l, fmt.Sprintf("  docker.io/library/app%d", i))
			}
			return append(l, "…")
		}()...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(cfg *Config) { cfg.ListImages = c.list; cfg.Pods = false; cfg.PVCs = false; cfg.Events = false })
			cluster(e, oneNode)
			e.Fake.Tool("crictl")
			e.Fake.Expect("crictl", c.out)
			labels := checktest.Labels(e.Run(Check{}))
			i := slices.Index(labels, "Container Images")
			var got []string
			if i >= 0 {
				got = labels[i:]
			}
			if !slices.Equal(got, c.wantRows) {
				t.Fatalf("image rows %q, want %q", got, c.wantRows)
			}
		})
	}
}

func TestImagesUseTheCRISocketAndAbsoluteCrictl(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	e.Fake.Tool("/var/lib/rancher/rke2/bin/crictl")
	e.Fake.Socket("/run/k3s/containerd/containerd.sock")
	e.Fake.Expect("crictl", crictlImages(1))
	rows := checktest.Rows(e.Run(Check{}))
	if !e.Fake.Ran("/var/lib/rancher/rke2/bin/crictl --runtime-endpoint unix:///run/k3s/containerd/containerd.sock images") {
		t.Fatalf("calls %q", e.Fake.Calls())
	}
	if rows["Container Images"].Value != "1 image(s) via crictl" {
		t.Fatalf("row %+v", rows["Container Images"])
	}
}

func TestNoCrictlSaysNothingAtAll(t *testing.T) {
	e := newEnv(t)
	cluster(e, oneNode)
	s := e.Run(Check{})
	if slices.Contains(checktest.Labels(s), "Container Images") {
		t.Fatal("Container Images row without crictl")
	}
	if len(s.MissingTools) != 0 {
		t.Fatalf("missing tools %+v", s.MissingTools)
	}
}
