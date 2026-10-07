// Package smtptest is a scripted in-process SMTP server for tests: it speaks
// just enough ESMTP (EHLO, STARTTLS, AUTH PLAIN/LOGIN, MAIL, RCPT, DATA,
// QUIT) to exercise a real client, and records what it received.
package smtptest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// Message is one accepted message.
type Message struct {
	From string
	To   []string
	Data string
	Auth string // "user:pass" when the client authenticated
	TLS  bool
}

// Server is a fake relay.
type Server struct {
	Addr string
	// Behaviour switches, set before the client connects.
	OfferSTARTTLS bool
	Implicit      bool   // TLS from the first byte
	AuthMechs     string // e.g. "PLAIN LOGIN"; empty = no AUTH offered
	RejectData    bool   // answer 554 after DATA
	RejectRcpt    bool   // answer 550 to RCPT

	ln     net.Listener
	tlsCfg *tls.Config
	mu     sync.Mutex
	msgs   []Message
	wg     sync.WaitGroup
}

// New starts a server on 127.0.0.1. Configure it with opts before any client
// connects; Close stops it.
func New(opts ...func(*Server)) (*Server, error) {
	s := &Server{}
	for _, o := range opts {
		o(s)
	}
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	s.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if s.Implicit {
		s.ln, err = tls.Listen("tcp", "127.0.0.1:0", s.tlsCfg)
	} else {
		s.ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	s.Addr = s.ln.Addr().String()
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Port is the listening port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Messages returns what was accepted so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// Close stops the server.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	isTLS := s.Implicit
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(lines ...string) {
		for _, l := range lines {
			_, _ = w.WriteString(l + "\r\n")
		}
		_ = w.Flush()
	}
	read := func() (string, bool) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	say("220 smtptest ready")
	var cur Message
	for {
		line, ok := read()
		if !ok {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		switch verb {
		case "EHLO", "HELO":
			ext := []string{"250-smtptest"}
			if s.OfferSTARTTLS && !isTLS {
				ext = append(ext, "250-STARTTLS")
			}
			if s.AuthMechs != "" {
				ext = append(ext, "250-AUTH "+s.AuthMechs)
			}
			ext = append(ext, "250 8BITMIME")
			say(ext...)
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, isTLS = tc, true
			r, w = bufio.NewReader(tc), bufio.NewWriter(tc)
		case "AUTH":
			f := strings.Fields(line)
			switch {
			case len(f) >= 3 && strings.EqualFold(f[1], "PLAIN"):
				b, _ := base64.StdEncoding.DecodeString(f[2])
				parts := strings.Split(string(b), "\x00")
				if len(parts) == 3 {
					cur.Auth = parts[1] + ":" + parts[2]
				}
				say("235 ok")
			case len(f) >= 2 && strings.EqualFold(f[1], "LOGIN"):
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
				u, _ := read()
				say("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
				p, _ := read()
				ub, _ := base64.StdEncoding.DecodeString(u)
				pb, _ := base64.StdEncoding.DecodeString(p)
				cur.Auth = string(ub) + ":" + string(pb)
				say("235 ok")
			default:
				say("504 unsupported")
			}
		case "MAIL":
			cur.From = addr(line)
			say("250 ok")
		case "RCPT":
			if s.RejectRcpt {
				say("550 no such user")
				continue
			}
			cur.To = append(cur.To, addr(line))
			say("250 ok")
		case "DATA":
			say("354 end with .")
			var b strings.Builder
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".") + "\r\n")
			}
			if s.RejectData {
				say("554 rejected")
				cur = Message{}
				continue
			}
			cur.Data, cur.TLS = b.String(), isTLS
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			cur = Message{Auth: cur.Auth}
			say("250 queued")
		case "RSET", "NOOP":
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

func addr(line string) string {
	i, j := strings.Index(line, "<"), strings.LastIndex(line, ">")
	if i < 0 || j < i {
		return ""
	}
	return line[i+1 : j]
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
