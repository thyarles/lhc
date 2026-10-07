package smtp

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/thyarles/lhc-go/internal/notify"
	"github.com/thyarles/lhc-go/internal/notify/smtp/smtptest"
)

var when = time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC)

func msg(mode string) notify.Message {
	return notify.Message{
		From: "lhc@web01.example.com", To: []string{"ops@example.com", "team@example.com"},
		Subject: "[daily] ⚠ CAUTION · web01 · 2026-01-05 — all clear",
		Text:    "plain body\nline two\n", HTML: "<p>html body</p>", HTMLMode: mode,
	}
}

func server(t *testing.T, opts ...func(*smtptest.Server)) *smtptest.Server {
	t.Helper()
	s, err := smtptest.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func sender(s *smtptest.Server, tlsMode string) *Sender {
	return &Sender{Config: Config{
		Host: "127.0.0.1", Port: s.Port(), TLS: tlsMode, TLSSkipVerify: true,
		HelloName: "web01.example.com", Timeout: 5 * time.Second,
	}, Now: func() time.Time { return when }}
}

func TestPlainDelivery(t *testing.T) {
	s := server(t)
	if err := sender(s, "none").Send(context.Background(), msg("inline")); err != nil {
		t.Fatal(err)
	}
	got := s.Messages()
	if len(got) != 1 || got[0].From != "lhc@web01.example.com" || len(got[0].To) != 2 || got[0].TLS {
		t.Fatalf("%+v", got)
	}
}

func TestSTARTTLSWithPlainAuth(t *testing.T) {
	s := server(t, func(s *smtptest.Server) { s.OfferSTARTTLS = true; s.AuthMechs = "PLAIN LOGIN" })
	snd := sender(s, "starttls")
	snd.Username, snd.Password = "user", "secret"
	if err := snd.Send(context.Background(), msg("inline")); err != nil {
		t.Fatal(err)
	}
	got := s.Messages()
	if len(got) != 1 || !got[0].TLS || got[0].Auth != "user:secret" {
		t.Fatalf("%+v", got)
	}
}

func TestImplicitTLSWithLoginAuth(t *testing.T) {
	// Some Exchange relays offer LOGIN and not PLAIN.
	s := server(t, func(s *smtptest.Server) { s.Implicit = true; s.AuthMechs = "LOGIN" })
	snd := sender(s, "implicit")
	snd.Username, snd.Password = "user", "secret"
	if err := snd.Send(context.Background(), msg("inline")); err != nil {
		t.Fatal(err)
	}
	if got := s.Messages(); len(got) != 1 || !got[0].TLS || got[0].Auth != "user:secret" {
		t.Fatalf("%+v", got)
	}
}

func TestSTARTTLSRequiredButNotOffered(t *testing.T) {
	s := server(t)
	if err := sender(s, "starttls").Send(context.Background(), msg("inline")); err == nil {
		t.Fatal("sent without the TLS the config asked for")
	}
}

func TestRejectedMessageIsAnError(t *testing.T) {
	// The caller commits the alert history only on success; a refusal must
	// surface so the alert is retried next run.
	s := server(t, func(s *smtptest.Server) { s.RejectData = true })
	if err := sender(s, "none").Send(context.Background(), msg("inline")); err == nil {
		t.Fatal("rejection reported as success")
	}
}

func TestRejectedRecipientIsAnError(t *testing.T) {
	s := server(t, func(s *smtptest.Server) { s.RejectRcpt = true })
	if err := sender(s, "none").Send(context.Background(), msg("inline")); err == nil {
		t.Fatal("rejection reported as success")
	}
}

func TestUnreachableRelayIsAnError(t *testing.T) {
	s := server(t)
	port := s.Port()
	s.Close()
	snd := &Sender{Config: Config{Host: "127.0.0.1", Port: port, TLS: "none", Timeout: time.Second}}
	if err := snd.Send(context.Background(), msg("inline")); err == nil {
		t.Fatal("no error from a closed port")
	}
}

// ── MIME structure ──────────────────────────────────────────────────────────

type mimePart struct {
	ctype, disposition, body string
}

func parse(t *testing.T, raw []byte) (*mail.Message, []mimePart) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var parts []mimePart
	var walk func(r io.Reader, ctype string)
	walk = func(r io.Reader, ctype string) {
		mt, params, err := mime.ParseMediaType(ctype)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(mt, "multipart/") {
			return
		}
		parts = append(parts, mimePart{ctype: mt})
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ct := p.Header.Get("Content-Type")
			if strings.HasPrefix(ct, "multipart/") {
				walk(p, ct)
				continue
			}
			b, _ := io.ReadAll(p) // multipart.Reader decodes quoted-printable
			mt, _, _ := mime.ParseMediaType(ct)
			parts = append(parts, mimePart{mt, p.Header.Get("Content-Disposition"), string(b)})
		}
	}
	walk(m.Body, m.Header.Get("Content-Type"))
	return m, parts
}

func kinds(parts []mimePart) string {
	var out []string
	for _, p := range parts {
		k := p.ctype
		if strings.HasPrefix(p.disposition, "attachment") {
			k += "(attachment)"
		}
		out = append(out, k)
	}
	return strings.Join(out, " ")
}

func TestMIMELayouts(t *testing.T) {
	cases := map[string]string{
		// Least preferred first, so a client that can render HTML shows it.
		"inline":     "multipart/alternative text/plain text/html",
		"attachment": "multipart/mixed text/plain text/html(attachment)",
		"both":       "multipart/mixed multipart/alternative text/plain text/html text/html(attachment)",
		"bogus":      "multipart/alternative text/plain text/html",
	}
	for mode, want := range cases {
		raw, err := Build(msg(mode), when)
		if err != nil {
			t.Fatal(err)
		}
		_, parts := parse(t, raw)
		if got := kinds(parts); got != want {
			t.Errorf("%s: %s, want %s", mode, got, want)
		}
	}
}

func TestBodiesSurviveQuotedPrintable(t *testing.T) {
	m := msg("inline")
	m.Text = strings.Repeat("é long line ", 20) + "\n= sign\n"
	raw, err := Build(m, when)
	if err != nil {
		t.Fatal(err)
	}
	_, parts := parse(t, raw)
	if got := strings.ReplaceAll(parts[1].body, "\r\n", "\n"); got != m.Text {
		t.Fatalf("text body changed:\n%q\n%q", got, m.Text)
	}
	for _, l := range strings.Split(string(raw), "\r\n") {
		if len(l) > 998 {
			t.Fatal("line over the RFC 5322 limit")
		}
	}
}

func TestSubjectIsEncodedAndDecodesBack(t *testing.T) {
	raw, err := Build(msg("inline"), when)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := parse(t, raw)
	enc := m.Header.Get("Subject")
	if !strings.HasPrefix(enc, "=?utf-8?q?") {
		t.Fatalf("subject not RFC 2047 encoded: %q", enc)
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(enc)
	if err != nil || dec != msg("inline").Subject {
		t.Fatalf("decoded %q (%v)", dec, err)
	}
	if m.Header.Get("Message-ID") == "" || m.Header.Get("Date") == "" {
		t.Fatal("missing Message-ID or Date")
	}
}
