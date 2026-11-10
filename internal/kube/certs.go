package kube

import (
	"crypto/x509"
	"encoding/pem"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/runner"
)

// rke2 and k3s issue their node certificates for a year and renew every one
// that is close to expiry, but only when the service starts: a node that is
// never restarted runs into it. The files are the source of truth, readable
// on workers that have no kubeconfig, and do not depend on events that the
// apiserver keeps for an hour.

// RenewWithin is how close to expiry a certificate must be for rke2 and k3s
// to renew it on start. Older releases used 90 days.
const RenewWithin = 120 * 24 * time.Hour

// Guard rails for the directory walk: server/tls holds a few dozen files.
const (
	certMaxDepth = 3
	certMaxFiles = 300
)

var certRoots = []struct{ distro, root string }{
	{"rke2", "/var/lib/rancher/rke2"},
	{"k3s", "/var/lib/rancher/k3s"},
}

// Cert is one leaf certificate on disk.
type Cert struct {
	File     string // relative to the distro root: server/tls/client-admin.crt
	NotAfter time.Time
}

// Base is the file name without its directory.
func (c Cert) Base() string { return path.Base(c.File) }

// Certs is what one rke2 or k3s install has on disk.
type Certs struct {
	Distro     string // rke2 | k3s; "" when neither is installed
	Server     bool   // server/tls exists: a control-plane node
	Leaves     []Cert // sorted by expiry, soonest first
	Unreadable int    // .crt files that could not be read or parsed
}

// Found says whether rke2 or k3s certificates are on this host.
func (c Certs) Found() bool { return c.Distro != "" && len(c.Leaves)+c.Unreadable > 0 }

// Service is the systemd unit whose restart renews the certificates.
func (c Certs) Service() string {
	switch {
	case c.Distro == "k3s" && c.Server:
		return "k3s"
	case c.Distro == "k3s":
		return "k3s-agent"
	case c.Server:
		return c.Distro + "-server"
	default:
		return c.Distro + "-agent"
	}
}

// Expired is the leaves already past their NotAfter at now.
func (c Certs) Expired(now time.Time) []Cert {
	var out []Cert
	for _, l := range c.Leaves {
		if !l.NotAfter.After(now) {
			out = append(out, l)
		}
	}
	return out
}

// ReadCerts reads the leaf certificates of the first rke2 or k3s install
// found: everything under server/tls and the *.crt directly in agent/ (the
// rest of agent/ is images and containerd state). CA certificates are
// skipped: they last ten years and a restart does not renew them. Only the
// first certificate of a file counts, so a chain file is not read as its CA.
func ReadCerts(r runner.Runner) Certs {
	for _, d := range certRoots {
		var c Certs
		tls := d.root + "/server/tls"
		if _, err := r.ReadDir(tls); err == nil {
			c.Server = true
			walkCerts(r, d.root, "server/tls", 0, &c)
		}
		walkCerts(r, d.root, "agent", certMaxDepth, &c)
		if len(c.Leaves)+c.Unreadable == 0 {
			continue
		}
		c.Distro = d.distro
		slices.SortStableFunc(c.Leaves, func(a, b Cert) int { return a.NotAfter.Compare(b.NotAfter) })
		return c
	}
	return Certs{}
}

func walkCerts(r runner.Runner, root, rel string, depth int, c *Certs) {
	entries, err := r.ReadDir(root + "/" + rel)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(c.Leaves)+c.Unreadable >= certMaxFiles {
			return
		}
		p := rel + "/" + e.Name()
		if e.IsDir() {
			if depth < certMaxDepth {
				walkCerts(r, root, p, depth+1, c)
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".crt") {
			continue
		}
		cert, ok := firstCert(r, root+"/"+p)
		switch {
		case !ok:
			c.Unreadable++
		case !cert.IsCA:
			c.Leaves = append(c.Leaves, Cert{File: p, NotAfter: cert.NotAfter.UTC()})
		}
	}
}

func firstCert(r runner.Runner, p string) (*x509.Certificate, bool) {
	b, err := r.ReadFile(p)
	if err != nil {
		return nil, false
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, false
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		return cert, err == nil
	}
}
