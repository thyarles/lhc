// Package network reports traffic per interface and a socket summary.
package network

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/thyarles/lhc-go/internal/check"
)

func init() { check.Register(Check{}) }

type Config struct {
	check.Toggle `yaml:",inline"`
}

type Check struct{}

func (Check) Meta() check.Meta { return check.Meta{Name: "network", Title: "Network I/O", Order: 200} }

func (Check) Defaults() check.Config { return &Config{Toggle: check.On} }

type iface struct {
	name   string
	rx, tx int64
}

// parseNetDev reads /proc/net/dev: two header lines, then
// "name: rx_bytes rx_packets ... (8 receive fields) tx_bytes ...". The name
// is cut at the colon rather than split on spaces because old kernels glue a
// large counter to it ("eth0:123456789").
func parseNetDev(b []byte) []iface {
	lines := strings.Split(string(b), "\n")
	var out []iface
	for _, line := range lines[min(2, len(lines)):] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "lo" || len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseInt(f[0], 10, 64)
		tx, err2 := strconv.ParseInt(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, iface{name, rx, tx})
	}
	return out
}

const ssLines = 4

func (c Check) Run(ctx context.Context, env *check.Env) *check.Section {
	s := check.NewSection("network", c.Meta().Title)
	r := env.Runner

	if b, err := r.ReadFile("/proc/net/dev"); err == nil {
		for _, i := range parseNetDev(b) {
			s.Add(i.name, fmt.Sprintf("RX %s  TX %s", fmtBytes(i.rx), fmtBytes(i.tx)), check.Info)
		}
	}

	if _, ok := r.LookPath("ss"); !ok {
		s.NeedTool(check.Tool{Name: "ss", RHELPkg: "iproute", DebPkg: "iproute2", SUSEPkg: "iproute2"})
		return s
	}
	// Only the head of `ss -s`: total sockets and the TCP state counts.
	lines := strings.Split(r.Run(ctx, "ss", "-s").Stdout, "\n")
	var summary []string
	for _, line := range lines[:min(ssLines, len(lines))] {
		if line = strings.TrimSpace(line); line != "" {
			summary = append(summary, line)
		}
	}
	if len(summary) > 0 {
		s.Separator("Connection Summary")
		for _, line := range summary {
			s.Add("", line, check.Info)
		}
	}
	return s
}

// fmtBytes matches the Python report: integer-divide by 1024 until the value
// fits, then print it with no decimals ("1 GB", not "1.5 GB").
func fmtBytes(n int64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if n > -1024 && n < 1024 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		n = floorDiv(n, 1024)
	}
	return fmt.Sprintf("%d PB", n)
}

// floorDiv is Python's //, which rounds towards minus infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
