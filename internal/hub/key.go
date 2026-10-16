// Package hub carries reports from hosts to collectors: the shared key,
// the encrypted envelope, the sender that runs after every `lhc run`, and
// the receiver and store behind `lhc collect`.
//
// Data flows one way. A collector answers 204 or 400 with an empty body and
// never sends anything else back, so a host never acts on what it hears.
package hub

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is the AES-256 key length.
const KeySize = 32

// NewKey returns a fresh key, base64-encoded with a trailing newline, the
// way a key file holds it.
func NewKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(k) + "\n"), nil
}

// LoadKey reads a key file. A file others can read is refused: whoever holds
// the key can read every host's report and forge new ones.
func LoadKey(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %04o): chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKey(b)
}

// ParseKey decodes a key file's contents.
func ParseKey(b []byte) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, errors.New("the hub key is not base64 (make one with `lhc hub keygen`)")
	}
	if len(k) != KeySize {
		return nil, fmt.Errorf("the hub key is %d bytes, want %d (make one with `lhc hub keygen`)", len(k), KeySize)
	}
	return k, nil
}
