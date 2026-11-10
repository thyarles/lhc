package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/report"
	"github.com/thyarles/lhc/internal/state"
)

// A claim is a host asking the collectors for its turn at something that
// must not happen on two hosts of a group the same day: today, restarting
// rke2/k3s to renew its certificates. The host acts only on a majority of
// all its collectors, and each collector grants a group once a day, so two
// majorities cannot exist on the same day. A rota with takeover, as for the
// digest, could: a host that cannot reach the first collector would ask the
// second while another host still reaches the first.
//
// This is the one place a collector answers with more than a status: the
// answer is sealed with the hub key and carries the claim's random ID, so a
// forged or replayed "granted" cannot make a host act.

// ClaimPath is where a host asks for its turn.
const ClaimPath = "/v1/claim"

// KindCertRestart is a claim to restart rke2/k3s.
const KindCertRestart = "cert-restart"

// claimKinds are the kinds this collector decides on.
var claimKinds = []string{KindCertRestart}

const (
	// grantGap is the least time between two grants of one group: two
	// dates, but the same night, around midnight.
	grantGap = 20 * time.Hour
	// grantKeep is how long grants are remembered.
	grantKeep = 60 * 24 * time.Hour
	// answerLimit caps a sealed answer.
	answerLimit = 64 << 10
)

// Claim is the sealed body of POST /v1/claim.
type Claim struct {
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Group  string `json:"group"`
	// ID is random; the answer carries it back.
	ID string `json:"id"`
	// Probe asks what the collector would answer, and records nothing.
	Probe bool `json:"probe,omitempty"`
}

// Answer is a collector's sealed reply to a claim.
type Answer struct {
	Schema  int    `json:"schema"`
	ID      string `json:"id"`
	By      string `json:"by"`
	Granted bool   `json:"granted"`
	Reason  string `json:"reason"`
}

// Reply is what one collector made of a claim, as the host sees it. Err is
// set when there is no usable answer: unreachable, refused the request, an
// older lhc. Each counts as a refusal.
type Reply struct {
	Collector string
	Answer    Answer
	Err       error
}

// Granted says whether the collector granted the claim.
func (r Reply) Granted() bool { return r.Err == nil && r.Answer.Granted }

// Text is the reply for the log: "granted", "refused: …" or the error.
func (r Reply) Text() string {
	switch {
	case r.Err != nil:
		return r.Err.Error()
	case r.Answer.Granted:
		return "granted"
	}
	return "refused: " + r.Answer.Reason
}

// Majority is how many of n collectors must grant: more than half.
func Majority(n int) int { return n/2 + 1 }

// Grants counts the replies that granted.
func Grants(replies []Reply) int {
	n := 0
	for _, r := range replies {
		if r.Granted() {
			n++
		}
	}
	return n
}

var groupName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)

// NewClaimID is a random claim ID.
func NewClaimID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// errNoClaims is a collector that does not take claims: an older lhc.
var errNoClaims = errors.New("does not take claims (older lhc): counts as a refusal until it is upgraded")

// Ask sends a claim to every collector and reads each sealed answer.
func Ask(ctx context.Context, key []byte, collectors []string, c Claim, now func() time.Time, timeout time.Duration) []Reply {
	body, err := json.Marshal(c)
	if err == nil {
		var e *Envelope
		if e, err = Seal(key, c.Host, now(), body); err == nil {
			body, err = json.Marshal(e)
		}
	}
	out := make([]Reply, 0, len(collectors))
	for _, col := range collectors {
		r := Reply{Collector: col, Err: err}
		if err == nil {
			r.Answer, r.Err = ask(ctx, key, col, body, c.ID, now, timeout)
		}
		out = append(out, r)
	}
	return out
}

func ask(ctx context.Context, key []byte, collector string, body []byte, id string, now func() time.Time, timeout time.Duration) (Answer, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(collector, "/")+ClaimPath, bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return Answer{}, ue.Err
		}
		return Answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, answerLimit+1))
	if err != nil {
		return Answer{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Answer{}, errNoClaims
	case http.StatusBadRequest:
		return Answer{}, fmt.Errorf("refused the claim (400): wrong hub key or clocks more than %s apart; the collector's log says which", MaxSkew)
	default:
		return Answer{}, fmt.Errorf("answered %s: is this an lhc collector?", resp.Status)
	}
	if len(raw) > answerLimit {
		return Answer{}, errors.New("answer too large")
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Answer{}, fmt.Errorf("answer is not an envelope: %w", err)
	}
	sent, err := time.Parse(time.RFC3339, e.SentAt)
	if err != nil {
		return Answer{}, fmt.Errorf("answer sent_at %q: %w", e.SentAt, err)
	}
	if skew := now().Sub(sent); skew > MaxSkew || skew < -MaxSkew {
		return Answer{}, fmt.Errorf("answer is %s from this host's clock (limit %s); check NTP on both", skew.Round(time.Second), MaxSkew)
	}
	plain, err := Open(key, &e, answerLimit)
	if err != nil {
		return Answer{}, fmt.Errorf("answer: %w", err)
	}
	var a Answer
	if err := json.Unmarshal(plain, &a); err != nil {
		return Answer{}, fmt.Errorf("answer is not JSON: %w", err)
	}
	if a.Schema != 1 || a.ID != id || a.By != e.Host {
		return Answer{}, errors.New("answer does not match the claim (replayed or altered)")
	}
	return a, nil
}

// readClaim checks an opened claim against its envelope.
func readClaim(e *Envelope, plain []byte) (Claim, error) {
	var c Claim
	if err := json.Unmarshal(plain, &c); err != nil {
		return c, fmt.Errorf("%s: the claim is not JSON: %w", e.Host, err)
	}
	switch {
	case c.Schema != 1:
		return c, fmt.Errorf("%s: claim schema %d, want 1", e.Host, c.Schema)
	case c.Host != e.Host:
		return c, fmt.Errorf("%s: the claim inside is for %q", e.Host, c.Host)
	case !slices.Contains(claimKinds, c.Kind):
		return c, fmt.Errorf("%s: unknown claim kind %q", e.Host, c.Kind)
	case !groupName.MatchString(c.Group):
		return c, fmt.Errorf("%s: bad group %q", e.Host, c.Group)
	case c.ID == "" || len(c.ID) > 64:
		return c, fmt.Errorf("%s: bad claim ID", e.Host)
	}
	return c, nil
}

// Grant is one claim a collector granted.
type Grant struct {
	Kind  string    `json:"kind"`
	Group string    `json:"group"`
	Host  string    `json:"host"`
	At    time.Time `json:"at"`
	Date  string    `json:"date"` // the collector's local date at At
}

func (s Store) claimsPath() string { return filepath.Join(s.Dir, ".claims.json") }

// LoadGrants reads the grants of the last 60 days. A missing or unreadable
// file holds none.
func (s Store) LoadGrants() []Grant {
	var g struct {
		Grants []Grant `json:"grants"`
	}
	if b, err := os.ReadFile(s.claimsPath()); err == nil {
		_ = json.Unmarshal(b, &g)
	}
	return g.Grants
}

func (s Store) saveGrants(gs []Grant) error {
	b, err := json.Marshal(struct {
		Grants []Grant `json:"grants"`
	}{gs})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	return state.WriteFileAtomic(s.claimsPath(), b, 0o600)
}

// Decide answers a claim and, when it is granted and not a probe, records
// the grant. Callers serialise it: the receiver holds a lock.
//
// A claim is granted only when this collector has granted no other host of
// the group today or in the last 20h, every earlier grant in the group was
// followed by a report from that host, and no other host of the group is
// SILENT. Asking again after a grant (a lost answer) is granted again.
func (s Store) Decide(c Claim, now time.Time, silentAfter time.Duration) (bool, string, error) {
	today := now.Format("2006-01-02")
	var kept, group []Grant
	for _, g := range s.LoadGrants() {
		if now.Sub(g.At) > grantKeep {
			continue
		}
		kept = append(kept, g)
		if g.Kind == c.Kind && g.Group == c.Group {
			group = append(group, g)
		}
	}
	entries, err := s.List()
	if err != nil {
		return false, "", err
	}
	byHost := map[string]Entry{}
	for _, e := range entries {
		byHost[e.Host] = e
	}

	// Newest first: the latest grant of each host is the one that counts.
	slices.SortFunc(group, func(a, b Grant) int { return b.At.Compare(a.At) })
	seen := map[string]bool{}
	for _, g := range group {
		switch {
		case g.Date == today && g.Host == c.Host:
			return true, "already granted to " + c.Host + " today", nil
		case g.Date == today:
			return false, g.Host + " restarts today (granted at " + g.At.In(now.Location()).Format("15:04") + ")", nil
		case now.Sub(g.At) < grantGap:
			return false, fmt.Sprintf("%s was granted at %s, less than %s ago", g.Host, g.At.In(now.Location()).Format("2006-01-02 15:04"), grantGap), nil
		}
		if seen[g.Host] || g.Host == c.Host {
			seen[g.Host] = true
			continue
		}
		seen[g.Host] = true
		if e, ok := byHost[g.Host]; !ok || !e.ReceivedAt.After(g.At) {
			return false, fmt.Sprintf("%s has not reported since its restart at %s", g.Host, g.At.In(now.Location()).Format("2006-01-02 15:04")), nil
		}
	}
	for _, e := range entries {
		if e.Host == c.Host || e.CertRestart == nil || e.CertRestart.Group != c.Group {
			continue
		}
		if now.Sub(e.ReceivedAt) > silentAfter {
			return false, e.Host + " is SILENT (no report for over " + report.Hours(silentAfter) + ")", nil
		}
	}
	if c.Probe {
		return true, "would be granted", nil
	}
	kept = append(kept, Grant{Kind: c.Kind, Group: c.Group, Host: c.Host, At: now, Date: today})
	if err := s.saveGrants(kept); err != nil {
		return false, "", fmt.Errorf("recording the grant: %w", err)
	}
	return true, "granted", nil
}
