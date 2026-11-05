package hub

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/check"
	"github.com/thyarles/lhc/internal/kube"
	"github.com/thyarles/lhc/internal/state"
	"github.com/thyarles/lhc/internal/textwrap"
)

// Owner mail: when one workload has too many OOM kills across the hub, the
// collector that sends the digest also mails the people the workload names
// in an annotation. Annotations can be set by anyone who can edit the
// workload, so only addresses in the SMTP domain are ever mailed.

const (
	// OwnersListed is how many addresses of one annotation are used.
	OwnersListed = 5
	// ownerWindow: only reports received this recently count, so a host's
	// old report does not add yesterday's kills again.
	ownerWindow = 24 * time.Hour
	// ownerSlack lets a digest sent a little earlier in the day than the
	// last one (a takeover the week before) still count as a full period.
	ownerSlack = 12 * time.Hour
	// ownerForget is how long a workload's last mail is kept after it
	// stopped being mailed.
	ownerForget = 30 * 24 * time.Hour
)

// MailDomain is the only domain owners may be mailed at: the domain of
// smtp.from when it is set, otherwise smtp.host without its first label
// (relay.example.com gives example.com). source says which one; both are
// "" when neither gives a domain, such as a blank from and an IP address.
func MailDomain(from, host string) (domain, source string) {
	if from != "" {
		a, err := mail.ParseAddress(from)
		if err != nil {
			return "", ""
		}
		_, d, _ := strings.Cut(a.Address, "@")
		if d = strings.ToLower(d); validDomain(d) {
			return d, "smtp.from"
		}
		return "", ""
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if net.ParseIP(host) != nil {
		return "", ""
	}
	if _, d, ok := strings.Cut(host, "."); ok && validDomain(d) {
		return d, "smtp.host"
	}
	return "", ""
}

// validDomain is a name with at least two labels, each of letters,
// digits and inner hyphens.
func validDomain(d string) bool {
	labels := strings.Split(d, ".")
	if len(labels) < 2 || len(d) > 253 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// Recipients reads an annotation's comma-separated entries. A bare name
// gets @domain; name@domain, ignoring case, is used as written; anything
// else is skipped and said why, quoted so a line break in it cannot reach
// a log line. Only the first OwnersListed entries are used.
func Recipients(value, domain string) (to, skipped []string) {
	n := 0
	for raw := range strings.SplitSeq(value, ",") {
		e := strings.Trim(raw, " \t")
		if e == "" {
			continue
		}
		n++
		if n > OwnersListed {
			skipped = append(skipped, fmt.Sprintf("%q skipped (only the first %d entries are used)", e, OwnersListed))
			continue
		}
		addr, why := recipient(e, domain)
		if why != "" {
			skipped = append(skipped, fmt.Sprintf("%q skipped (%s)", e, why))
			continue
		}
		if !slices.ContainsFunc(to, func(t string) bool { return strings.EqualFold(t, addr) }) {
			to = append(to, addr)
		}
	}
	return to, skipped
}

func recipient(e, domain string) (string, string) {
	if strings.ContainsFunc(e, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", "has a space or a line break"
	}
	local, d, hasAt := strings.Cut(e, "@")
	addr := e
	switch {
	case !hasAt:
		addr = e + "@" + domain
	case !strings.EqualFold(d, domain):
		return "", "not @" + domain
	}
	if local == "" || len(local) > 64 || strings.ContainsFunc(local, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._%+-", r)
	}) {
		return "", "not a name of letters, digits and ._%+-"
	}
	if a, err := mail.ParseAddress(addr); err != nil || a.Address != addr || a.Name != "" {
		return "", "not an address"
	}
	return addr, ""
}

// OwnerPod is one pod of a workload, as its owners read about it.
type OwnerPod struct {
	Node, Namespace, Name, Container string
	Limit                            string // "none" when not set; "" when not known
	Kills                            int
	Last                             time.Time
	annotation                       string // the pod's own annotation
	note                             string // its digest line's key
}

// Workload is one Deployment, StatefulSet or DaemonSet (or a pod that
// belongs to none of them, or no longer exists) and its OOM kills today.
type Workload struct {
	Key   string // "Deployment team-a/api", "Pod team-a/debug"
	Kills int
	Pods  []OwnerPod
	// To are the addresses to mail, from Source: the workload, a pod or
	// the namespace whose annotation named them. Skipped are the entries
	// that cannot be mailed, and why.
	To      []string
	Skipped []string
	Source  string
	ns      string
}

// Name is the workload's namespace/name.
func (w Workload) Name() string {
	_, n, _ := strings.Cut(w.Key, " ")
	return n
}

// OwnerLookup is what PlanOwners works from.
type OwnerLookup struct {
	Pods map[string]kube.Pod // the cluster's pods by UID
	// Workloads ("Deployment team-a/api" → annotation) and Namespaces
	// (name → annotation); Workloads is nil before they were read, and
	// then no address is looked up.
	Workloads  map[string]string
	Namespaces map[string]string
	Annotation string
	Domain     string
	MinKills   int
	Now        time.Time
}

// PlanOwners groups the OOM-killed pods of every report received in the
// last 24h by workload, and returns the workloads with at least MinKills
// kills, most first, with the addresses their annotation gives. A host
// kill carries no pod, so a node out of memory mails no application team.
func PlanOwners(entries []Entry, l OwnerLookup) []Workload {
	byName := map[string]string{}
	for uid, p := range l.Pods {
		byName[p.Namespace+"/"+p.Name] = uid
	}
	groups := map[string]*Workload{}
	for _, e := range entries {
		if e.Err != nil || l.Now.Sub(e.ReceivedAt) > ownerWindow {
			continue
		}
		for _, r := range e.Pods {
			if r.Reason != "oom_killed" {
				continue
			}
			uid := r.UID
			if _, ok := l.Pods[uid]; !ok {
				uid = byName[r.Namespace+"/"+r.Name]
			}
			p, found := l.Pods[uid]
			op := OwnerPod{Node: e.Host, Namespace: r.Namespace, Name: r.Name, Container: r.Container, Kills: r.Count, Last: r.Last, note: noteKey(e.Host, r)}
			key := ""
			if found {
				op.Namespace, op.Name, op.annotation = p.Namespace, p.Name, p.Annotations[l.Annotation]
				if p.Node != "" {
					op.Node = p.Node
				}
				if op.Container == "" && len(p.Containers) == 1 {
					for c := range p.Containers {
						op.Container = c
					}
				}
				if c, ok := p.Containers[op.Container]; ok {
					op.Limit = cmp.Or(c.Limit, "none")
				}
				key = p.Owner
			}
			if op.Namespace == "" {
				continue // a pod known only by its UID, and gone: nothing to go by
			}
			if key == "" && !found {
				if d := guessDeployment(op.Namespace, op.Name); d != "" {
					if _, ok := l.Workloads[d]; ok || l.Workloads == nil {
						key = d
					}
				}
			}
			if key == "" {
				key = "Pod " + op.Namespace + "/" + op.Name
			}
			w := groups[key]
			if w == nil {
				w = &Workload{Key: key, ns: op.Namespace}
				groups[key] = w
			}
			w.Kills += op.Kills
			w.Pods = append(w.Pods, op)
		}
	}
	var out []Workload
	for _, w := range groups {
		if l.MinKills <= 0 || w.Kills < l.MinKills {
			continue
		}
		slices.SortStableFunc(w.Pods, func(a, b OwnerPod) int {
			return cmp.Or(cmp.Compare(b.Kills, a.Kills), strings.Compare(a.Name, b.Name))
		})
		if l.Workloads != nil {
			w.address(l)
		}
		out = append(out, *w)
	}
	slices.SortFunc(out, func(a, b Workload) int { return cmp.Or(cmp.Compare(b.Kills, a.Kills), strings.Compare(a.Key, b.Key)) })
	return out
}

// address finds the first annotation that is set: the workload's, then a
// pod's, then the namespace's. Only that one is read, even when none of
// its entries can be mailed.
func (w *Workload) address(l OwnerLookup) {
	value := l.Workloads[w.Key]
	w.Source = w.Key
	if value == "" {
		w.Source = ""
		for _, p := range w.Pods {
			if p.annotation != "" {
				value, w.Source = p.annotation, "pod "+p.Namespace+"/"+p.Name
				break
			}
		}
	}
	if value == "" {
		if value = l.Namespaces[w.ns]; value != "" {
			w.Source = "namespace " + w.ns
		}
	}
	w.To, w.Skipped = Recipients(value, l.Domain)
}

// guessDeployment is the Deployment a vanished pod belonged to, from its
// name: api-6b7c9d8f5-x2k4p is Deployment api, whose ReplicaSet added the
// pod-template hash and the pod's own suffix.
func guessDeployment(ns, name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return ""
	}
	suffix, hash := parts[len(parts)-1], parts[len(parts)-2]
	if len(suffix) != 5 || !lowerAlnum(suffix) || len(hash) < 5 || len(hash) > 10 || !lowerAlnum(hash) {
		return ""
	}
	return "Deployment " + ns + "/" + strings.Join(parts[:len(parts)-2], "-")
}

func lowerAlnum(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') })
}

// noteKey ties a host's PodRef to its digest line.
func noteKey(host string, r check.PodRef) string {
	return host + "\x00" + r.UID + "\x00" + r.Namespace + "/" + r.Name
}

// ownerNotes is what each listed pod's digest line says about its owners.
func ownerNotes(ws []Workload) map[string]string {
	notes := map[string]string{}
	for _, w := range ws {
		note := "no owner named"
		if len(w.To) > 0 {
			note = "told " + strings.Join(w.To, ", ")
		}
		for _, p := range w.Pods {
			notes[p.note] = note
		}
	}
	return notes
}

// OwnerState is <store>/.owner-notify.json: when each workload's owners
// were last mailed.
type OwnerState struct {
	Mailed map[string]time.Time `json:"mailed"`
}

func (s Store) ownersPath() string { return filepath.Join(s.Dir, ".owner-notify.json") }

// LoadOwners reads the state; a missing or unreadable file is "never mailed".
func (s Store) LoadOwners() OwnerState {
	var st OwnerState
	if b, err := os.ReadFile(s.ownersPath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Mailed == nil {
		st.Mailed = map[string]time.Time{}
	}
	return st
}

// SaveOwners records the state.
func (s Store) SaveOwners(st OwnerState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	return state.WriteFileAtomic(s.ownersPath(), b, 0o600)
}

// Next is when the workload may be mailed again; zero when it never was.
func (st OwnerState) Next(key string, remind time.Duration) time.Time {
	last, ok := st.Mailed[key]
	if !ok {
		return time.Time{}
	}
	return last.Add(remind - ownerSlack)
}

// Due says whether the workload's owners should be mailed at now.
func (st OwnerState) Due(key string, now time.Time, remind time.Duration) bool {
	next := st.Next(key, remind)
	return next.IsZero() || !now.Before(next)
}

// Forget drops the workloads last mailed long enough ago that a new mail
// would be due anyway, and says whether any was dropped.
func (st OwnerState) Forget(now time.Time, remind time.Duration) bool {
	keep := max(ownerForget, remind+ownerWindow)
	n := len(st.Mailed)
	for k, t := range st.Mailed {
		if now.Sub(t) > keep {
			delete(st.Mailed, k)
		}
	}
	return len(st.Mailed) != n
}

// OwnerMailInfo is the rest of what an owner mail says.
type OwnerMailInfo struct {
	Annotation string
	Remind     time.Duration
	Version    string
	Now        time.Time
}

// OwnerMail is the subject and plain-text body of one workload's mail:
// its own pods and nothing about any other workload or host.
func OwnerMail(w Workload, o OwnerMailInfo) (subject, text string) {
	subject = fmt.Sprintf("[lhc] OOM kills: %s · %d today", w.Name(), w.Kills)
	var b strings.Builder
	para := func(s string) {
		for _, l := range textwrap.Wrap(s, 72, "") {
			b.WriteString(l + "\n")
		}
		b.WriteString("\n")
	}
	para(fmt.Sprintf("%s had %d OOM kills today: each time, a container reached its memory limit and the kernel killed one of its processes.", w.Name(), w.Kills))
	for _, p := range w.Pods {
		parts := []string{p.Namespace + "/" + p.Name}
		if p.Container != "" {
			parts = append(parts, "container "+p.Container)
		}
		parts = append(parts, killCount(p.Kills))
		if p.Limit != "" {
			parts = append(parts, "limit "+p.Limit)
		}
		if !p.Last.IsZero() {
			parts = append(parts, "last "+p.Last.In(o.Now.Location()).Format("15:04"))
		}
		parts = append(parts, "node "+p.Node)
		// One line per pod, however long, as in the digest.
		b.WriteString("  " + strings.Join(parts, " · ") + "\n")
	}
	b.WriteString("\n")
	para("When the killed process is not the container's main process, such as a PHP or Python worker, the container keeps running: Kubernetes shows no restart and no OOMKilled, and the work that process was doing is lost. Raise the memory limit, or lower the number of workers or the memory each one uses.")
	para(fmt.Sprintf("You get this mail because the annotation %s on %s names you. Remove your name from it to stop these mails. While the kills go on, a reminder follows every %s.", o.Annotation, w.Source, days(o.Remind)))
	b.WriteString("-- \nlhc " + o.Version + "\n")
	return subject, b.String()
}

func killCount(n int) string {
	if n == 1 {
		return "1 kill"
	}
	return fmt.Sprintf("%d kills", n)
}

// days is 7 days, 1 day, or 36h for a period that is not whole days.
func days(d time.Duration) string {
	switch {
	case d == 24*time.Hour:
		return "day"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	return d.String()
}
