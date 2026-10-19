package apikey

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestGenerateFormat(t *testing.T) {
	for _, env := range []Env{EnvLive, EnvTest} {
		t.Run(string(env), func(t *testing.T) {
			k := Generate(env)
			prefix := PrefixFor(env)

			if k.Env != env {
				t.Fatalf("Env = %q, want %q", k.Env, env)
			}
			if !strings.HasPrefix(k.Plaintext, prefix) {
				t.Fatalf("expected prefix %q, got %q", prefix, k.Plaintext)
			}
			wantLen := len(prefix) + encodedLength + checksumLength
			if len(k.Plaintext) != wantLen {
				t.Fatalf("expected length %d, got %d", wantLen, len(k.Plaintext))
			}
			if !Valid(k.Plaintext) {
				t.Fatal("generated key is not Valid")
			}
			if !ValidForEnv(k.Plaintext, env) {
				t.Fatal("generated key is not ValidForEnv")
			}
			if k.Hash != Hash(k.Plaintext) {
				t.Fatal("Hash field does not match Hash(plaintext)")
			}
			if len(k.Hash) != 64 {
				t.Fatalf("expected 64 hex chars, got %d", len(k.Hash))
			}
			if !strings.HasPrefix(k.Plaintext, k.KeyPrefix) || len(k.KeyPrefix) != len(prefix)+displayChars {
				t.Fatalf("bad KeyPrefix %q", k.KeyPrefix)
			}
		})
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		k := Generate(EnvTest)
		if seen[k.Plaintext] {
			t.Fatal("duplicate key generated")
		}
		seen[k.Plaintext] = true
	}
}

func TestHashIsDeterministic(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := Hash("abc"); got != want {
		t.Fatalf("Hash(abc) = %s, want %s", got, want)
	}
	if Hash("rly_test_abc") == Hash("rly_test_abd") {
		t.Fatal("different inputs produced the same hash")
	}
}

func TestValid(t *testing.T) {
	good := Generate(EnvTest).Plaintext
	live := Generate(EnvLive).Plaintext
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"generated test key", good, true},
		{"generated live key", live, true},
		{"empty", "", false},
		{"legacy rly_ only", "rly_" + good[len(PrefixFor(EnvTest)):], false},
		{"no prefix", good[len(PrefixFor(EnvTest)):], false},
		{"wrong brand", "sk_test_" + good[len(PrefixFor(EnvTest)):], false},
		{"too short", good[:len(good)-1], false},
		{"too long", good + "A", false},
		{"invalid base64 char", flipChar(good, len(good)-checksumLength-1, '!'), false},
		{"bad checksum", flipChar(good, len(good)-1, nextBase62(good[len(good)-1])), false},
		{"whitespace", " " + good[1:], false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Valid(tt.in); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidForEnv(t *testing.T) {
	testKey := Generate(EnvTest).Plaintext
	liveKey := Generate(EnvLive).Plaintext

	if !ValidForEnv(testKey, EnvTest) || ValidForEnv(testKey, EnvLive) {
		t.Fatal("test key env mismatch")
	}
	if !ValidForEnv(liveKey, EnvLive) || ValidForEnv(liveKey, EnvTest) {
		t.Fatal("live key env mismatch")
	}
}

func TestEnvOf(t *testing.T) {
	k := Generate(EnvLive)
	got, ok := EnvOf(k.Plaintext)
	if !ok || got != EnvLive {
		t.Fatalf("EnvOf = %q,%v want live,true", got, ok)
	}
	if _, ok := EnvOf("not-a-key"); ok {
		t.Fatal("EnvOf accepted garbage")
	}
}

func TestChecksumCatchesTypos(t *testing.T) {
	k := Generate(EnvTest).Plaintext
	// Flip one entropy character; checksum must fail without DB.
	typo := flipChar(k, len(PrefixFor(EnvTest))+3, nextBase62(k[len(PrefixFor(EnvTest))+3]))
	if Valid(typo) {
		t.Fatalf("typo still Valid: %s", typo)
	}
}

func TestKeyNeverPrintsPlaintext(t *testing.T) {
	k := Generate(EnvTest)

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("created", "key", k)

	outputs := map[string]string{
		"%v":     fmt.Sprintf("%v", k),
		"%+v":    fmt.Sprintf("%+v", k),
		"%#v":    fmt.Sprintf("%#v", k),
		"String": k.String(),
		"slog":   logs.String(),
	}
	for verb, out := range outputs {
		if strings.Contains(out, k.Plaintext) {
			t.Fatalf("%s leaks the plaintext key: %s", verb, out)
		}
	}
}

func flipChar(s string, i int, repl byte) string {
	b := []byte(s)
	b[i] = repl
	return string(b)
}

func nextBase62(c byte) byte {
	i := strings.IndexByte(base62Alphabet, c)
	if i < 0 {
		return base62Alphabet[0]
	}
	return base62Alphabet[(i+1)%len(base62Alphabet)]
}
