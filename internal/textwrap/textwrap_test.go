package textwrap

import (
	"strings"
	"testing"
)

func TestWrapSplitsWordsLongerThanTheColumn(t *testing.T) {
	out := Wrap(strings.Repeat("x", 200), 40, "")
	for _, l := range out {
		if Len(l) > 40 {
			t.Fatalf("%d cols: %q", Len(l), l)
		}
	}
	if !strings.Contains(strings.Join(out, ""), "…") {
		t.Fatal("split word not marked")
	}
}

func TestWrapHandlesEmptyInput(t *testing.T) {
	if got := Wrap("", 40, ""); len(got) != 1 || got[0] != "" {
		t.Fatalf("got %q", got)
	}
}

func TestWrapIndentsContinuationLines(t *testing.T) {
	got := Wrap("aaa bbb ccc", 7, "  ")
	if len(got) != 2 || got[0] != "aaa bbb" || got[1] != "  ccc" {
		t.Fatalf("got %q", got)
	}
}

func TestMeterFillsProportionally(t *testing.T) {
	for pct, filled := range map[float64]int{0: 0, 50: 5, 91: 9, 100: 10} {
		bar := Meter(pct)
		if strings.Count(bar, "#") != filled || len(bar) != 12 {
			t.Errorf("Meter(%v) = %q", pct, bar)
		}
	}
}

func TestMeterClampsOutOfRangeValues(t *testing.T) {
	for _, pct := range []float64{-20, 150} {
		if n := strings.Count(Meter(pct), "#"); n < 0 || n > 10 {
			t.Errorf("Meter(%v) = %q", pct, Meter(pct))
		}
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	if got := Truncate("ééééé", 3); got != "éé…" {
		t.Fatalf("got %q", got)
	}
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestFitHostKeepsTheLeadingLabel(t *testing.T) {
	host := "srv0123456789.subdomain-that-goes-on.internal.example.cloudapp.net"
	line := FitHost("  Linux Health Check vdev  ·  {host}", host, 78)
	if Len(line) > 78 || !strings.Contains(line, "srv0123456789") {
		t.Fatalf("%d cols: %q", Len(line), line)
	}
	if short := FitHost("x {host}", "web01.example.com", 78); short != "x web01.example.com" {
		t.Fatalf("short host changed: %q", short)
	}
}
