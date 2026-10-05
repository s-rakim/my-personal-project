// Package secret handles subscriber credential storage and bearer tokens.
//
// RADIUS PAP hands the server the subscriber's cleartext password (recovered
// from the request), so the password store does not need to be reversible: hash
// it. This matters because a WISP's subscriber database is a password database,
// and subscribers reuse passwords.
//
// PBKDF2-HMAC-SHA256 is implemented here rather than imported to keep the
// daemon dependency-free. It is RFC 8018 section 5.2 and short enough to audit
// at a glance.
package secret

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultIterations is the PBKDF2 work factor. Raise it over time; stored
// hashes carry their own iteration count so old records keep verifying.
const DefaultIterations = 600_000

const (
	saltLen = 16
	keyLen  = 32
)

// ErrMalformed means a stored hash could not be parsed.
var ErrMalformed = errors.New("secret: malformed stored hash")

// Hash derives a storable representation of a password. The format is
// pbkdf2-sha256$<iterations>$<salt-b64>$<key-b64>.
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secret: read entropy: %w", err)
	}
	key := pbkdf2SHA256([]byte(password), salt, DefaultIterations, keyLen)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		DefaultIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches stored. The comparison is
// constant-time.
func Verify(stored, password string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, ErrMalformed
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false, ErrMalformed
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, ErrMalformed
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, ErrMalformed
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Token returns a URL-safe random token, used for terminal and admin API
// bearer credentials.
func Token(nBytes int) (string, error) {
	if nBytes <= 0 {
		nBytes = 32
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secret: read entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// EqualConstantTime compares two tokens without leaking length-independent
// timing. Use it for every credential check on a request path.
func EqualConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// pbkdf2SHA256 implements PBKDF2 with HMAC-SHA256 as the PRF.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	out := make([]byte, 0, blocks*hashLen)
	u := make([]byte, 0, hashLen)
	block := make([]byte, 4)

	for i := 1; i <= blocks; i++ {
		prf.Reset()
		prf.Write(salt)
		block[0] = byte(i >> 24)
		block[1] = byte(i >> 16)
		block[2] = byte(i >> 8)
		block[3] = byte(i)
		prf.Write(block)
		u = prf.Sum(u[:0])

		t := make([]byte, hashLen)
		copy(t, u)

		for j := 1; j < iter; j++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
