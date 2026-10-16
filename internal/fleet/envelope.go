package fleet

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"
)

// Version is the envelope format.
const Version = 1

// Envelope is the body of POST /v1/report. Data is the gzipped report JSON,
// sealed with AES-256-GCM. GCM authenticates as well as encrypts: without
// the key a report can be neither read nor forged, which is why plain HTTP
// is enough. Reports list ports, users and packages, so they are encrypted,
// not just signed.
type Envelope struct {
	V      int    `json:"v"`
	Host   string `json:"host"`
	SentAt string `json:"sent_at"` // RFC 3339, UTC
	Nonce  []byte `json:"nonce"`   // 12 bytes; base64 in JSON
	Data   []byte `json:"data"`
}

// hostName is what a host may call itself: it becomes a file name on the
// collector, so no slashes, and no leading dot (hidden and temporary files).
var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// ValidHost reports whether name can travel in an envelope.
func ValidHost(name string) bool { return hostName.MatchString(name) }

// aad binds the header to the ciphertext, so a header cannot be moved onto
// another host's report.
func (e *Envelope) aad() []byte {
	return []byte(strconv.Itoa(e.V) + "|" + e.Host + "|" + e.SentAt)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal compresses and encrypts one report.
func Seal(key []byte, host string, sentAt time.Time, report []byte) (*Envelope, error) {
	if !ValidHost(host) {
		return nil, fmt.Errorf("host name %q cannot be sent: use letters, digits, '.', '_' and '-' (set hostname: in the config)", host)
	}
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(report); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	e := &Envelope{V: Version, Host: host, SentAt: sentAt.UTC().Format(time.RFC3339), Nonce: make([]byte, aead.NonceSize())}
	if _, err := rand.Read(e.Nonce); err != nil {
		return nil, err
	}
	e.Data = aead.Seal(nil, e.Nonce, buf.Bytes(), e.aad())
	return e, nil
}

// errTooLarge is a report that inflates past the limit: a gzip bomb, or a
// report far bigger than any real host writes.
var errTooLarge = errors.New("report larger than collector.max_report_size once decompressed")

// Open authenticates, decrypts and decompresses an envelope, reading at most
// limit bytes of report.
func Open(key []byte, e *Envelope, limit int64) ([]byte, error) {
	if e.V != Version {
		return nil, fmt.Errorf("envelope version %d", e.V)
	}
	if !ValidHost(e.Host) {
		return nil, fmt.Errorf("bad host name %q", e.Host)
	}
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(e.Nonce) != aead.NonceSize() {
		return nil, errors.New("bad nonce")
	}
	plain, err := aead.Open(nil, e.Nonce, e.Data, e.aad())
	if err != nil {
		return nil, errors.New("does not decrypt: wrong key, or altered on the way")
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("not gzip: %w", err)
	}
	report, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	if int64(len(report)) > limit {
		return nil, errTooLarge
	}
	return report, nil
}
