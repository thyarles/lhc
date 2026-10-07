// Package host answers "which machine is this, and how does it install
// software?" without asking any check to work it out.
package host

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// Label is the name a report is about.
//
// NOT the resolved FQDN. Resolving the kernel hostname through /etc/hosts and
// DNS returns the FIRST name found, which on a clustered host is routinely a
// shared VIP: three RKE2 nodes behind cluster-vip.example.com each reported
// themselves as cluster-vip.example.com, so their reports, subject lines and saved
// files were indistinguishable and their alerts looked like one host flapping.
// The answer is not even stable — it follows whichever node holds the VIP.
//
// So the kernel's own name wins. The resolver is consulted for one narrow
// purpose — adding a domain the kernel name lacks — and only when it is
// talking about the same machine (same first label, any case).
//
// override is the config's hostname: setting, which beats everything.
func Label(override, kernel string, resolve func() string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	if strings.Contains(kernel, ".") {
		return kernel
	}
	fqdn := resolve()
	if kernel != "" && fqdn != "" && strings.EqualFold(firstLabel(fqdn), kernel) {
		return fqdn
	}
	if kernel != "" {
		return kernel
	}
	return fqdn
}

// MailDomain is the host part of the default From: address. A bare short
// name is rejected by some relays, so borrow the resolved name's domain when
// there is one. Still distinct per host, which is the point.
func MailDomain(label string, resolve func() string) string {
	if strings.Contains(label, ".") {
		return label
	}
	fqdn := resolve()
	if _, domain, ok := strings.Cut(fqdn, "."); ok && domain != "" {
		return label + "." + domain
	}
	return label
}

func firstLabel(name string) string {
	first, _, _ := strings.Cut(name, ".")
	return first
}

// Kernel returns the kernel's hostname, or "" if it cannot be read.
func Kernel() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// Resolve is the equivalent of Python's socket.getfqdn(): look the name up,
// reverse-resolve the first address, and return the first answer that
// contains a dot; otherwise the name itself. Bounded to two seconds so a dead
// resolver cannot hang a scheduled run.
func Resolve(name string) string {
	if name == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var r net.Resolver
	addrs, err := r.LookupHost(ctx, name)
	if err != nil || len(addrs) == 0 {
		return name
	}
	names, err := r.LookupAddr(ctx, addrs[0])
	if err != nil {
		return name
	}
	for _, n := range names {
		n = strings.TrimSuffix(n, ".")
		if strings.Contains(n, ".") {
			return n
		}
	}
	return name
}

// Cached wraps a resolver so it is asked at most once per process.
func Cached(f func() string) func() string {
	var (
		done bool
		v    string
	)
	return func() string {
		if !done {
			v, done = f(), true
		}
		return v
	}
}

// ParseOSRelease reads the KEY="value" lines of /etc/os-release.
func ParseOSRelease(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

// PkgManager picks the package manager, in the order the Python version
// did, with zypper last: dnf, yum, apt-get, zypper.
func PkgManager(lookPath func(string) (string, bool)) string {
	for _, pm := range []string{"dnf", "yum", "apt-get", "zypper"} {
		if _, ok := lookPath(pm); ok {
			return pm
		}
	}
	return ""
}

// InstallCmd is the command that installs pkg with pm, or "" when no
// package manager was detected.
func InstallCmd(pm, pkg string) string {
	switch pm {
	case "dnf", "yum", "apt-get":
		return pm + " install -y " + pkg
	case "zypper":
		return "zypper --non-interactive install " + pkg
	}
	return ""
}
