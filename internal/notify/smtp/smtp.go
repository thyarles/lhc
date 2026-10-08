// Package smtp delivers a report through an SMTP relay: plain, STARTTLS or
// implicit TLS, with optional PLAIN or LOGIN authentication.
package smtp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/thyarles/lhc/internal/notify"
)

// Config is the smtp: block of the config.
type Config struct {
	Host          string
	Port          int
	TLS           string // none | starttls | implicit
	TLSSkipVerify bool
	Username      string
	Password      string
	// HelloName is what we announce in EHLO; usually this host's name.
	HelloName string
	Timeout   time.Duration
}

// Sender is the SMTP Notifier.
type Sender struct {
	Config
	Now func() time.Time
}

var _ notify.Notifier = (*Sender)(nil)

// Send delivers m. A nil error means the relay accepted the message for
// every recipient.
func (s *Sender) Send(ctx context.Context, m notify.Message) error {
	if len(m.To) == 0 {
		return errors.New("no recipients")
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	host := s.Host
	if host == "" {
		host = "localhost"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.Port))
	tlsCfg := &tls.Config{ServerName: host, InsecureSkipVerify: s.TLSSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in for relays with self-signed certificates

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if s.TLS == "implicit" {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("tls %s: %w", addr, err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	hello := s.HelloName
	if hello == "" {
		hello = "localhost"
	}
	if err := c.Hello(hello); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if s.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS", addr)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(chooseAuth(c, s.Username, s.Password, host)); err != nil {
			return fmt.Errorf("AUTH: %w", err)
		}
	}
	if err := c.Mail(m.From); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, r := range m.To {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	body, err := Build(m, now())
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message refused: %w", err)
	}
	// The message is accepted at this point; a failed QUIT changes nothing.
	_ = c.Quit()
	return nil
}

func chooseAuth(c *smtp.Client, user, pass, host string) smtp.Auth {
	if ok, mechs := c.Extension("AUTH"); ok && !strings.Contains(strings.ToUpper(mechs), "PLAIN") &&
		strings.Contains(strings.ToUpper(mechs), "LOGIN") {
		return loginAuth{user, pass}
	}
	return smtp.PlainAuth("", user, pass, host)
}

// loginAuth is the LOGIN mechanism, which some Exchange relays offer instead
// of PLAIN. net/smtp only ships PLAIN and CRAM-MD5.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("refusing LOGIN over an unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge %q", fromServer)
}

// Build renders the RFC 5322 message: headers plus a MIME body with
// quoted-printable parts, CRLF line endings throughout.
func Build(m notify.Message, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	domain := "localhost"
	if _, d, ok := strings.Cut(m.From, "@"); ok {
		domain = strings.Trim(d, "> ")
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)

	h := func(k, v string) { buf.WriteString(k + ": " + v + "\r\n") }
	h("From", m.From)
	h("To", strings.Join(m.To, ", "))
	// The subject carries ⚠ and ·, so it must be RFC 2047 encoded.
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("X-Mailer", "lhc")

	mode := m.HTMLMode
	switch mode {
	case "attachment", "both":
	default:
		mode = "inline"
	}

	outer := multipart.NewWriter(&buf)
	kind := "multipart/mixed"
	if mode == "inline" {
		kind = "multipart/alternative"
	}
	h("Content-Type", kind+"; boundary="+outer.Boundary())
	buf.WriteString("\r\n")

	switch mode {
	case "attachment":
		if err := part(outer, "text/plain", m.Text, false); err != nil {
			return nil, err
		}
	case "both":
		boundary := multipart.NewWriter(nil).Boundary()
		w, err := outer.CreatePart(textproto.MIMEHeader{"Content-Type": {"multipart/alternative; boundary=" + boundary}})
		if err != nil {
			return nil, err
		}
		inner := multipart.NewWriter(w)
		if err := inner.SetBoundary(boundary); err != nil {
			return nil, err
		}
		if err := alternative(inner, m); err != nil {
			return nil, err
		}
		if err := inner.Close(); err != nil {
			return nil, err
		}
	default:
		if err := alternative(outer, m); err != nil {
			return nil, err
		}
	}
	if mode != "inline" {
		if err := part(outer, "text/html", m.HTML, true); err != nil {
			return nil, err
		}
	}
	if err := outer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// alternative writes text first and HTML last: least preferred first, per
// RFC 2046, so a client that can render HTML shows it.
func alternative(w *multipart.Writer, m notify.Message) error {
	if err := part(w, "text/plain", m.Text, false); err != nil {
		return err
	}
	return part(w, "text/html", m.HTML, false)
}

func part(w *multipart.Writer, ctype, body string, attach bool) error {
	hdr := textproto.MIMEHeader{
		"Content-Type":              {ctype + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	}
	if attach {
		hdr.Set("Content-Disposition", `attachment; filename="health-report.html"`)
	}
	pw, err := w.CreatePart(hdr)
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(pw)
	if _, err := qp.Write([]byte(body)); err != nil {
		return err
	}
	return qp.Close()
}
