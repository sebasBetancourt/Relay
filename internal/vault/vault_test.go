package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func newVault(t *testing.T) *Vault {
	t.Helper()
	v, err := New(newKey(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return v
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	v := newVault(t)
	aad := AAD("oauth_tokens", "acc-1", "access_token")

	for _, plaintext := range [][]byte{[]byte("ya29.a0AfH6SM-token"), {}} {
		ct, nonce, err := v.Encrypt(plaintext, aad)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if len(plaintext) > 0 && bytes.Contains(ct, plaintext) {
			t.Fatal("ciphertext contains the plaintext")
		}
		got, err := v.Decrypt(ct, nonce, aad)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("got %q, want %q", got, plaintext)
		}
	}
}

func TestEncryptSamePlaintextDiffers(t *testing.T) {
	v := newVault(t)
	plaintext := []byte("same token")

	ct1, n1, _ := v.Encrypt(plaintext, nil)
	ct2, n2, _ := v.Encrypt(plaintext, nil)
	if bytes.Equal(n1, n2) {
		t.Fatal("nonce repeated")
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("same plaintext produced the same ciphertext")
	}
	if len(n1) != 12 {
		t.Fatalf("nonce length = %d, want 12", len(n1))
	}
	// Ciphertext = encrypted bytes + 16-byte GCM tag.
	if len(ct1) != len(plaintext)+16 {
		t.Fatalf("ciphertext length = %d, want %d", len(ct1), len(plaintext)+16)
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	v := newVault(t)
	aad := AAD("oauth_tokens", "acc-1", "access_token")
	ct, nonce, err := v.Encrypt([]byte("refresh-token-value"), aad)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(b []byte, i int) []byte {
		c := bytes.Clone(b)
		c[i] ^= 0x01
		return c
	}
	other, err := New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		v         *Vault
		ct, nonce []byte
		aad       []byte
	}{
		{"ciphertext byte flipped", v, flip(ct, 0), nonce, aad},
		{"tag byte flipped", v, flip(ct, len(ct)-1), nonce, aad},
		{"ciphertext truncated", v, ct[:len(ct)-1], nonce, aad},
		{"nonce flipped", v, ct, flip(nonce, 0), aad},
		{"other row", v, ct, nonce, AAD("oauth_tokens", "acc-2", "access_token")},
		{"other column", v, ct, nonce, AAD("oauth_tokens", "acc-1", "refresh_token")},
		{"missing aad", v, ct, nonce, nil},
		{"wrong key", other, ct, nonce, aad},
		{"short nonce must not panic", v, ct, nonce[:8], aad},
		{"empty nonce must not panic", v, ct, nil, aad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.v.Decrypt(tt.ct, tt.nonce, tt.aad)
			if !errors.Is(err, ErrDecrypt) {
				t.Fatalf("err = %v, want ErrDecrypt", err)
			}
			if got != nil {
				t.Fatalf("returned data on failure: %q", got)
			}
		})
	}
}

func TestNewRejectsBadKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		key := bytes.Repeat([]byte{0xAB}, size)
		_, err := New(key)
		if err == nil {
			t.Fatalf("size %d: expected error", size)
		}
		if size > 0 && strings.Contains(err.Error(), hex.EncodeToString(key)) {
			t.Fatalf("error leaks the key: %v", err)
		}
	}
}

func TestVaultDoesNotLeakKey(t *testing.T) {
	key := newKey(t)
	v, err := New(key)
	if err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("oops", "vault", v)

	for _, out := range []string{fmt.Sprint(v), fmt.Sprintf("%v %+v %#v", v, v, v), logged.String()} {
		if strings.Contains(out, hex.EncodeToString(key)) || strings.Contains(out, string(key)) {
			t.Fatalf("key leaked in %q", out)
		}
		if !strings.Contains(out, "redacted") {
			t.Fatalf("expected redacted output, got %q", out)
		}
	}
}

func TestAADIsUnambiguous(t *testing.T) {
	if bytes.Equal(AAD("a|b", "c"), AAD("a", "b|c")) {
		t.Fatal("AAD collision between different part splits")
	}
	if bytes.Equal(AAD("ab", ""), AAD("a", "b")) {
		t.Fatal("AAD collision with empty part")
	}
	if !bytes.Equal(AAD("x", "y"), AAD("x", "y")) {
		t.Fatal("AAD must be deterministic")
	}
}
