package crypto

import "testing"

func TestSecretBoxRoundTrip(t *testing.T) {
	box, err := NewSecretBox("test-master-key")
	if err != nil {
		t.Fatalf("NewSecretBox() error = %v", err)
	}
	sealed, err := box.Encrypt("provider-secret")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if sealed == "provider-secret" {
		t.Fatal("encrypted value must not equal plaintext")
	}
	plain, err := box.Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if plain != "provider-secret" {
		t.Fatalf("plain = %q, want provider-secret", plain)
	}
}
