package kubernetes

import (
	"fmt"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/kube"
)

// daysLeft is whole days until t, never negative.
func daysLeft(now, t time.Time) int {
	return max(0, int(t.Sub(now)/(24*time.Hour)))
}

// certRows reports the expiry of the rke2/k3s certificates on this node and
// says whether it found any. It reads files only, so it runs on workers
// without a kubeconfig and while the API is down (an expired client
// certificate is one way the API goes down).
//
// Quiet by default: rke2 warns from 120 days on, hundreds of times a day;
// this stays INFO until certificates_caution_days, which only a node that
// was not restarted for months ever reaches.
func certRows(s *check.Section, env *check.Env, cfg *Config) bool {
	c := kube.ReadCerts(env.Runner)
	if !c.Found() {
		return false
	}
	now := env.Now().UTC()
	unreadable := ""
	if c.Unreadable > 0 {
		unreadable = fmt.Sprintf(" (%d unreadable)", c.Unreadable)
	}
	if len(c.Leaves) == 0 {
		s.Add("Certificates", fmt.Sprintf("no %s certificate could be read%s", c.Distro, unreadable), check.Info)
		return true
	}

	if expired := c.Expired(now); len(expired) > 0 {
		names := make([]string, 0, 3)
		for _, l := range firstN(expired, 3) {
			names = append(names, l.Base())
		}
		list := strings.Join(names, ", ") + moreSuffix(len(expired))
		s.Add("Certificates", fmt.Sprintf("%d of %d EXPIRED: %s", len(expired), len(c.Leaves), list), check.Unhealthy,
			check.Detail("restart "+c.Service()+" to renew them"))
		s.Alert(check.Unhealthy, fmt.Sprintf("%s certificates EXPIRED: %s", c.Distro, list))
		return true
	}

	// The date first: an INFO row gets one line of about 42 columns in the
	// text report, and the date is what a reader compares with rke2's event.
	first := c.Leaves[0]
	days := daysLeft(now, first.NotAfter)
	certs := fmt.Sprintf("%d certs", len(c.Leaves))
	if len(c.Leaves) == 1 {
		certs = "1 cert"
	}
	value := fmt.Sprintf("first expiry %s (%dd), %s", first.NotAfter.Format("2006-01-02"), days, certs)
	var detail []string
	if first.NotAfter.Sub(now) <= kube.RenewWithin {
		detail = append(detail, first.Base()+", renewed on the next "+c.Service()+" restart")
	} else {
		detail = append(detail, first.Base())
	}
	if c.Unreadable > 0 {
		detail = append(detail, fmt.Sprintf("%d unreadable", c.Unreadable))
	}
	opt := check.Detail(strings.Join(detail, "; "))
	if days < cfg.CertificatesCautionDays {
		s.Add("Certificates", value, check.Caution, opt)
		s.Alert(check.Caution, fmt.Sprintf("%s certificates expire in %dd: restart %s to renew them", c.Distro, days, c.Service()))
		return true
	}
	s.Add("Certificates", value, check.Info, opt)
	return true
}
