package fte_test

import (
	"testing"

	"github.com/redjack/marionette/fte"
)

func TestCipher(t *testing.T) {
	cipher, err := fte.NewCipher(`^(a|b|c)+$`, 512)
	if err != nil {
		t.Fatal(err)
	}
	defer cipher.Close()

	// Encode/decode first message.
	if ciphertext, err := cipher.Encrypt([]byte(`test`)); err != nil {
		t.Fatal(err)
	} else if plaintext, remainder, err := cipher.Decrypt(ciphertext); err != nil {
		t.Fatal(err)
	} else if string(plaintext) != `test` {
		t.Fatalf("unexpected plaintext: %q", plaintext)
	} else if string(remainder) != `` {
		t.Fatalf("unexpected remainder: %q", remainder)
	}

	// Encode/decode second message.
	if ciphertext, err := cipher.Encrypt([]byte(`foo bar`)); err != nil {
		t.Fatal(err)
	} else if plaintext, remainder, err := cipher.Decrypt(ciphertext); err != nil {
		t.Fatal(err)
	} else if string(plaintext) != `foo bar` {
		t.Fatalf("unexpected plaintext: %q", plaintext)
	} else if string(remainder) != `` {
		t.Fatalf("unexpected remainder: %q", remainder)
	}

	if err := cipher.Close(); err != nil {
		t.Fatal(err)
	}
}
