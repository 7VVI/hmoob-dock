package secret_test

import (
	"bytes"
	"testing"

	"hmoob-dock/internal/secret"
)

func TestCiphertextIsBoundToVersion(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal([]byte("PASSWORD=secret"), []byte("app-1/version-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open(ciphertext, []byte("app-2/version-1")); err == nil {
		t.Fatal("cross-application ciphertext accepted")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("DATABASE_URL=postgres://user:pass@db/app")
	ciphertext, err := box.Seal(plain, []byte("app-1/version-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plain) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := box.Open(ciphertext, []byte("app-1/version-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("plaintext = %q, want %q", got, plain)
	}
}

func TestSealUsesIndependentRandomNonce(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("app-1/version-1")
	first, err := box.Seal([]byte("same"), aad)
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.Seal([]byte("same"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two seals of the same plaintext produced identical ciphertext")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal([]byte("PASSWORD=secret"), []byte("app-1/version-1"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := secret.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(ciphertext, []byte("app-1/version-1")); err == nil {
		t.Fatal("ciphertext opened with the wrong key")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("app-1/version-1")
	ciphertext, err := box.Seal([]byte("PASSWORD=secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(ciphertext)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := box.Open(tampered, aad); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestOpenRejectsShortCiphertext(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open([]byte("short"), []byte("app-1/version-1")); err == nil {
		t.Fatal("short ciphertext accepted")
	}
	if _, err := box.Open(nil, []byte("app-1/version-1")); err == nil {
		t.Fatal("empty ciphertext accepted")
	}
}

func TestOpenRejectsMissingAAD(t *testing.T) {
	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal([]byte("PASSWORD=secret"), []byte("app-1/version-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Open(ciphertext, nil); err == nil {
		t.Fatal("ciphertext accepted without its additional data")
	}
}

func TestNewRejectsInvalidKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := secret.New(make([]byte, size)); err == nil {
			t.Fatalf("New accepted a %d byte key", size)
		}
	}
}
