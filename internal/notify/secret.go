package notify

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// HashSecret is the hex SHA-256 of a channel token or clientState.
// Only the hash is stored.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// SecretMatches reports whether presented hashes to storedHash.
// The comparison is constant time. A malformed stored hash does not match
// and still runs the compare, so a bad row is not a faster reject.
func SecretMatches(presented, storedHash string) bool {
	sum := sha256.Sum256([]byte(presented))
	want, err := hex.DecodeString(storedHash)
	if err != nil || len(want) != sha256.Size {
		var zero [sha256.Size]byte
		return subtle.ConstantTimeCompare(sum[:], zero[:]) == 1
	}
	return subtle.ConstantTimeCompare(sum[:], want) == 1
}

func newSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("subscription secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("subscription id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
