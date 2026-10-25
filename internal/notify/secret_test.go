package notify

import "testing"

func TestSecretMatches(t *testing.T) {
	const secret = "channel-token-0123456789abcdef"
	hash := HashSecret(secret)
	if hash == secret {
		t.Fatal("the stored value must be a hash, not the secret")
	}
	if !SecretMatches(secret, hash) {
		t.Fatal("the right secret must match")
	}
	// Same length, one different byte: still a full compare, not a prefix check.
	wrong := []byte(secret)
	wrong[0] ^= 0xff
	if SecretMatches(string(wrong), hash) {
		t.Fatal("a different secret must not match")
	}
	if SecretMatches(secret, "zz") || SecretMatches(secret, "") {
		t.Fatal("a malformed stored hash must not match")
	}
}
