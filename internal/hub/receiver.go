package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Path is the one route a collector answers.
const Path = "/v1/report"

const (
	// MaxSkew is how far sent_at may be from the collector's clock.
	MaxSkew = 10 * time.Minute
	// ReplayWindow is how long a nonce is remembered. It outlasts the
	// 2×MaxSkew span in which an envelope is acceptable at all, so a
	// replayed request is refused for as long as it could be fresh.
	ReplayWindow = 20 * time.Minute
	// RatePerMinute is the most requests one source address may make.
	RatePerMinute = 60
)

// Receiver is the HTTP handler behind `lhc collect`. Every failure answers
// 400 with an empty body: the reason goes to the collector's log, never to
// the client, so a probe learns nothing.
type Receiver struct {
	Store Store
	Now   func() time.Time
	Log   func(format string, args ...any)

	mu     sync.Mutex
	key    []byte
	limit  int64
	nonces map[string]time.Time  // nonce → when it was accepted
	hits   map[string]*rateCount // source IP → this minute's requests
	pruned time.Time
}

type rateCount struct {
	start time.Time
	n     int
}

// NewReceiver builds a receiver; limit caps a request and the report it
// inflates to.
func NewReceiver(key []byte, limit int64, store Store, now func() time.Time, log func(string, ...any)) *Receiver {
	r := &Receiver{Store: store, Now: now, Log: log, nonces: map[string]time.Time{}, hits: map[string]*rateCount{}}
	r.Configure(key, limit)
	return r
}

// Configure swaps the key and the size limit, for a reload on SIGHUP.
func (r *Receiver) Configure(key []byte, limit int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.key, r.limit = key, limit
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != Path || req.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		ip = req.RemoteAddr
	}
	host, overall, err := r.accept(w, req, ip)
	if err != nil {
		r.Log("rejected from %s: %v", ip, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.Log("accepted %s from %s (%s)", host, ip, overall)
	w.WriteHeader(http.StatusNoContent)
}

func (r *Receiver) accept(w http.ResponseWriter, req *http.Request, ip string) (host, overall string, err error) {
	now := r.Now()
	r.mu.Lock()
	key, limit := r.key, r.limit
	r.prune(now)
	allowed := r.count(ip, now)
	r.mu.Unlock()
	if !allowed {
		return "", "", fmt.Errorf("more than %d requests in a minute", RatePerMinute)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return "", "", fmt.Errorf("request larger than collector.max_report_size (%d bytes)", limit)
		}
		return "", "", fmt.Errorf("reading the request: %w", err)
	}
	var e Envelope
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return "", "", fmt.Errorf("not an envelope: %w", err)
	}
	sent, err := time.Parse(time.RFC3339, e.SentAt)
	if err != nil {
		return "", "", fmt.Errorf("sent_at %q: %w", e.SentAt, err)
	}
	if skew := now.Sub(sent); skew > MaxSkew || skew < -MaxSkew {
		return "", "", fmt.Errorf("%s: sent_at is %s from this collector's clock (limit %s); check NTP on both", e.Host, skew.Round(time.Second), MaxSkew)
	}
	// Decrypt before looking at the nonce: only a genuine envelope may take
	// a place in the replay cache.
	report, err := Open(key, &e, limit)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", e.Host, err)
	}
	var sum summary
	if err := json.Unmarshal(report, &sum); err != nil {
		return "", "", fmt.Errorf("%s: the report is not JSON: %w", e.Host, err)
	}
	if sum.Schema != 1 {
		return "", "", fmt.Errorf("%s: report schema %d, want 1", e.Host, sum.Schema)
	}
	if sum.Host != e.Host {
		return "", "", fmt.Errorf("%s: the report inside is for %q", e.Host, sum.Host)
	}

	r.mu.Lock()
	nonce := string(e.Nonce)
	_, replayed := r.nonces[nonce]
	if !replayed {
		r.nonces[nonce] = now
	}
	r.mu.Unlock()
	if replayed {
		return "", "", fmt.Errorf("%s: replayed request (nonce already seen)", e.Host)
	}

	if err := r.Store.Put(e.Host, Stored{ReceivedAt: now, RemoteAddr: ip, Report: report}); err != nil {
		return "", "", fmt.Errorf("%s: storing the report: %w", e.Host, err)
	}
	return e.Host, sum.Overall, nil
}

// count records one request from ip and reports whether it is within the
// rate. Called with r.mu held.
func (r *Receiver) count(ip string, now time.Time) bool {
	c := r.hits[ip]
	if c == nil || now.Sub(c.start) >= time.Minute {
		c = &rateCount{start: now}
		r.hits[ip] = c
	}
	c.n++
	return c.n <= RatePerMinute
}

// prune drops expired nonces and rate windows, at most once a minute, so the
// maps stay as small as the traffic of the last twenty minutes. Called with
// r.mu held.
func (r *Receiver) prune(now time.Time) {
	if now.Sub(r.pruned) < time.Minute {
		return
	}
	r.pruned = now
	for n, at := range r.nonces {
		if now.Sub(at) >= ReplayWindow {
			delete(r.nonces, n)
		}
	}
	for ip, c := range r.hits {
		if now.Sub(c.start) >= time.Minute {
			delete(r.hits, ip)
		}
	}
}
