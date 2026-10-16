package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SendTimeout bounds each collector, so a dead one costs a run seconds.
const SendTimeout = 10 * time.Second

// Outcome is what one collector made of a report. Err is nil when it
// accepted it.
type Outcome struct {
	Collector string
	Err       error
}

// Accepted counts the collectors that took the report.
func Accepted(outs []Outcome) int {
	n := 0
	for _, o := range outs {
		if o.Err == nil {
			n++
		}
	}
	return n
}

// client talks to collectors only: no proxy from the environment (they are
// on the internal network) and no redirects (a host never acts on what a
// collector says).
var client = &http.Client{
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Send delivers one sealed envelope to each collector in turn. The same
// envelope goes to all of them: each collector keeps its own replay cache.
func Send(ctx context.Context, collectors []string, e *Envelope, timeout time.Duration) []Outcome {
	body, err := json.Marshal(e)
	out := make([]Outcome, 0, len(collectors))
	for _, c := range collectors {
		if err == nil {
			out = append(out, Outcome{Collector: c, Err: post(ctx, c, body, timeout)})
		} else {
			out = append(out, Outcome{Collector: c, Err: err})
		}
	}
	return out
}

func post(ctx context.Context, collector string, body []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(collector, "/")+Path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// The log line names the collector already: drop the "Post <url>:".
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusBadRequest:
		// The collector never says why; its log does.
		return fmt.Errorf("refused (400): wrong hub key, clocks more than %s apart, or a report too large; the collector's log says which", MaxSkew)
	default:
		return fmt.Errorf("answered %s: is this an lhc collector?", resp.Status)
	}
}
