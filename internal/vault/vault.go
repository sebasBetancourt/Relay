// Package vault encrypts secrets at rest (OAuth tokens) with AES-256-GCM.
//
// Every Encrypt draws a fresh random 96-bit nonce, so the same plaintext
// never produces the same ciphertext. GCM appends a 16-byte authentication
// tag to the ciphertext: Decrypt verifies it before returning anything, so
// a tampered ciphertext, nonce, AAD or wrong key fails instead of yielding
// garbage.
//
// AAD (additional authenticated data) is not encrypted but is covered by
// the tag. Callers bind each ciphertext to where it lives, e.g.
// AAD("oauth_tokens", accountID, "access_token"), so a ciphertext copied to
// another row or column no longer decrypts.
//
// The key comes from configuration and is never logged or stored in the
// database: a DB dump alone must not be enough to read tokens.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
)

// KeySize is the key length AES-256 requires.
const KeySize = 32

// ErrDecrypt is returned for any decryption failure. It deliberately does
// not say whether the tag, nonce, AAD or key was wrong.
var ErrDecrypt = errors.New("vault: decryption failed")

// Vault holds the ready-to-use AEAD, not the raw key. It is safe for
// concurrent use: build one at startup and share it.
type Vault struct {
	aead cipher.AEAD
}

// New builds a Vault from a 32-byte key. Errors never include the key.
func New(key []byte) (*Vault, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("vault: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: new gcm: %w", err)
	}
	return &Vault{aead: aead}, nil
}

// Encrypt seals plaintext under a fresh random nonce. Store ciphertext
// (which ends with the 16-byte tag) and nonce together; the same aad must
// be passed to Decrypt.
func (v *Vault) Encrypt(plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("vault: nonce: %w", err)
	}
	return v.aead.Seal(nil, nonce, plaintext, aad), nonce, nil
}

// Decrypt verifies the tag and returns the plaintext, or ErrDecrypt.
func (v *Vault) Decrypt(ciphertext, nonce, aad []byte) ([]byte, error) {
	// cipher.AEAD.Open panics on a wrong nonce length; corrupt rows must
	// become an error, not crash the process.
	if len(nonce) != v.aead.NonceSize() {
		return nil, ErrDecrypt
	}
	plaintext, err := v.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// AAD joins parts unambiguously (each one length-prefixed), so
// AAD("a|b", "c") and AAD("a", "b|c") never collide.
func AAD(parts ...string) []byte {
	var out []byte
	for _, p := range parts {
		out = binary.AppendUvarint(out, uint64(len(p)))
		out = append(out, p...)
	}
	return out
}

// String, GoString and LogValue keep a Vault passed to fmt or slog by
// mistake from printing anything sensitive.
func (v *Vault) String() string       { return "vault{redacted}" }
func (v *Vault) GoString() string     { return v.String() }
func (v *Vault) LogValue() slog.Value { return slog.StringValue(v.String()) }
