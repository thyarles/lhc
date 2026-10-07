// Package logscan holds the log helpers shared by the auth and logs checks:
// finding the log files, and counting the lines that match a pattern TODAY.
//
// Everything is scoped to the current day. Grepping the whole log file made
// every check sticky: one segfault last month kept the report yellow until the
// log rotated, and the counts silently reset when it did.
package logscan

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc-go/internal/runner"
)

// MaxLines caps the matches Today returns per source. The log scan streams
// through grep and keeps only the tail, so a host under a flood of probes
// cannot make one check read millions of lines into memory.
const MaxLines = 200

// Samples is how many of the most recent matches Today hands back.
const Samples = 3

const (
	countTimeout = 30 * time.Second
	todayTimeout = 60 * time.Second
)

// AuthLog is the authentication log of this distribution family, or "" on a
// journald-only host.
func AuthLog(r runner.Runner) string {
	for _, p := range []string{"/var/log/auth.log", "/var/log/secure"} {
		if r.Exists(p) {
			return p
		}
	}
	return ""
}

// SystemLogs lists the system log files present on this host.
func SystemLogs(r runner.Runner) []string {
	var out []string
	for _, p := range []string{"/var/log/syslog", "/var/log/messages", "/var/log/kern.log", "/var/log/dmesg"} {
		if r.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// TodayRE is an extended regex matching now's date in syslog format. syslog
// pads a single-digit day with a space ("Jan  5"), not a zero, so the day is
// matched after any run of spaces and must be followed by one: "Jan 15" is
// not the 5th.
func TodayRE(now time.Time) string {
	return now.Format("Jan") + "[ ]+" + strconv.Itoa(now.Day()) + "[ ]"
}

// Quote makes s a single shell word. The patterns are constants, but a path
// or a configured pattern must never be able to break out of the pipeline.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// journal is the journalctl invocation for today. The date comes from now
// rather than journalctl's own "today", so the journal and the file fallback
// agree on which day it is.
func journal(now time.Time) string {
	return "journalctl --since=" + now.Format("2006-01-02") + " --no-pager -q 2>/dev/null"
}

// Count counts today's lines matching pattern (an extended regex, case
// sensitive). The journal is preferred when journalctl exists; otherwise file
// is grepped for today's date. A missing file counts zero.
func Count(ctx context.Context, r runner.Runner, now time.Time, file, pattern string) int {
	ctx, cancel := context.WithTimeout(ctx, countTimeout)
	defer cancel()
	var script string
	if _, ok := r.LookPath("journalctl"); ok {
		script = journal(now) + " | grep -cE " + Quote(pattern) + " || echo 0"
	} else {
		if file == "" || !r.Exists(file) {
			return 0
		}
		script = "grep -E " + Quote(TodayRE(now)) + " " + Quote(file) +
			" 2>/dev/null | grep -cE " + Quote(pattern) + " || echo 0"
	}
	return ParseCount(r.Shell(ctx, script).Stdout)
}

// ParseCount reads the first line of `grep -c ... || echo 0`. grep -c prints
// "0" and exits 1 when nothing matches, so the echo adds a second "0"; only
// the first line counts. Anything unreadable is zero.
func ParseCount(out string) int {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	n, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Today returns how many of today's lines match pattern (an extended regex,
// case insensitive) and the last few of them. With journalctl the journal is
// searched and sources are ignored; without it, each source that exists is
// grepped for today's date. Counts are capped at MaxLines per source.
//
// A pipeline rather than journalctl -g: RHEL 7's journalctl has no -g, and
// streaming through grep keeps a noisy day's volume out of memory.
func Today(ctx context.Context, r runner.Runner, now time.Time, sources []string, pattern string) (int, []string) {
	ctx, cancel := context.WithTimeout(ctx, todayTimeout)
	defer cancel()
	tail := " | grep -iE " + Quote(pattern) + " | tail -" + strconv.Itoa(MaxLines)
	var lines []string
	if _, ok := r.LookPath("journalctl"); ok {
		lines = nonBlank(r.Shell(ctx, journal(now)+tail).Stdout)
	} else {
		for _, src := range sources {
			if !r.Exists(src) {
				continue
			}
			out := r.Shell(ctx, "grep -E "+Quote(TodayRE(now))+" "+Quote(src)+" 2>/dev/null"+tail).Stdout
			lines = append(lines, nonBlank(out)...)
		}
	}
	return len(lines), lines[max(0, len(lines)-Samples):]
}

func nonBlank(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
