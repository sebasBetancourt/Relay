// Package apikey generates and hashes tenant API keys.
//
// Format (Stripe-style env + GitHub-style checksum):
//
//	rly_live_<43 base64url chars><6 base62 CRC32>
//	rly_test_<43 base64url chars><6 base62 CRC32>
//
// The CRC32 covers the 43-char entropy only, so a mistyped key fails Valid
// without hitting the database. Only the SHA-256 hash is stored; plaintext
// is shown to the tenant once.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"hash/crc32"
	"log/slog"
	"strings"
)

// Env selects the key prefix so test keys cannot be confused with production.
type Env string

const (
	EnvLive Env = "live"
	EnvTest Env = "test"
)

const (
	// Brand is the shared leading mark for secret scanners (rly_live_ / rly_test_).
	Brand = "rly_"

	randomBytes    = 32 // 256 bits of entropy: unguessable, so SHA-256 is enough.
	displayChars   = 8  // random chars kept in KeyPrefix for logs/UI.
	encodedLength  = 43 // base64.RawURLEncoding.EncodedLen(randomBytes)
	checksumLength = 6  // base62 CRC32, same idea as GitHub ghp_ tokens.
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// PrefixFor returns the full prefix for env, e.g. "rly_live_".
func PrefixFor(env Env) string {
	switch env {
	case EnvLive:
		return Brand + "live_"
	case EnvTest:
		return Brand + "test_"
	default:
		return Brand + "test_"
	}
}

// Key is a freshly generated API key. Plaintext must be shown once and never
// stored or logged; persist Hash and KeyPrefix only.
type Key struct {
	Plaintext string
	Hash      string
	KeyPrefix string
	Env       Env
}

// Generate returns a new random key for the given environment.
func Generate(env Env) Key {
	if env != EnvLive && env != EnvTest {
		env = EnvTest
	}

	b := make([]byte, randomBytes)
	// Since Go 1.24 crypto/rand.Read never fails; it crashes the program instead.
	_, _ = rand.Read(b)

	prefix := PrefixFor(env)
	entropy := base64.RawURLEncoding.EncodeToString(b)
	plaintext := prefix + entropy + checksum(entropy)

	return Key{
		Plaintext: plaintext,
		Hash:      Hash(plaintext),
		KeyPrefix: prefix + entropy[:displayChars],
		Env:       env,
	}
}

// Hash returns the hex SHA-256 of a plaintext key, as stored in tenants.api_key_hash.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Valid reports whether s has the shape of a Relay key and a correct checksum.
// It is a cheap pre-check to reject typos and garbage before touching the
// database; it does not prove the key exists.
func Valid(s string) bool {
	_, ok := parse(s)
	return ok
}

// ValidForEnv is Valid plus a hard check that the key matches env
// (reject rly_test_ keys when the process is live, and vice versa).
func ValidForEnv(s string, env Env) bool {
	parsed, ok := parse(s)
	return ok && parsed.env == env
}

// EnvOf returns the environment encoded in a well-formed key.
func EnvOf(s string) (Env, bool) {
	parsed, ok := parse(s)
	if !ok {
		return "", false
	}
	return parsed.env, true
}

type parsedKey struct {
	env     Env
	entropy string
}

func parse(s string) (parsedKey, bool) {
	var env Env
	switch {
	case strings.HasPrefix(s, PrefixFor(EnvLive)):
		env = EnvLive
	case strings.HasPrefix(s, PrefixFor(EnvTest)):
		env = EnvTest
	default:
		return parsedKey{}, false
	}

	prefix := PrefixFor(env)
	body := s[len(prefix):]
	if len(body) != encodedLength+checksumLength {
		return parsedKey{}, false
	}

	entropy := body[:encodedLength]
	gotSum := body[encodedLength:]
	if checksum(entropy) != gotSum {
		return parsedKey{}, false
	}

	raw, err := base64.RawURLEncoding.DecodeString(entropy)
	if err != nil || len(raw) != randomBytes {
		return parsedKey{}, false
	}
	return parsedKey{env: env, entropy: entropy}, true
}

// checksum is CRC32(IEEE) of the entropy string, encoded as 6 base62 digits
// (GitHub-style), so scanners and Valid can reject corrupt keys offline.
func checksum(entropy string) string {
	sum := crc32.ChecksumIEEE([]byte(entropy))
	return encodeBase62(sum, checksumLength)
}

func encodeBase62(n uint32, width int) string {
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = base62Alphabet[n%62]
		n /= 62
	}
	return string(buf)
}

// String, GoString and LogValue redact the plaintext so a Key passed to
// fmt or slog by mistake never leaks the secret.
func (k Key) String() string       { return k.KeyPrefix + "…" }
func (k Key) GoString() string     { return "apikey.Key{" + k.String() + "}" }
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }
