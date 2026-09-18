package vault

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	plaintext := []byte("DB_PASSWORD: hunter2\nAPI_KEY: abc123\n")
	sealed, err := Encrypt(plaintext, "correct horse")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("the plaintext is present in the encrypted file")
	}
	out, err := Decrypt(sealed, "correct horse")
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Errorf("round trip changed the content: %q", out)
	}
}

// TestAWrongPassphraseIsRefused, and the message does not claim to know which
// of the two possible causes it was — GCM cannot tell them apart.
func TestAWrongPassphraseIsRefused(t *testing.T) {
	sealed, err := Encrypt([]byte("secret: value\n"), "right")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(sealed, "wrong"); err == nil {
		t.Fatal("a wrong passphrase opened the vault")
	} else if !strings.Contains(err.Error(), "wrong passphrase") {
		t.Errorf("the error does not name the likely cause: %v", err)
	}
}

// TestTamperingIsDetected is the property a plain cipher would not give.
//
// AES-CTR without a MAC decrypts modified ciphertext into modified plaintext,
// silently: flipping a bit in an encrypted password flips a bit in the password
// that reaches a provider. GCM authenticates, so the file refuses to open.
func TestTamperingIsDetected(t *testing.T) {
	sealed, err := Encrypt([]byte("DB_PASSWORD: hunter2\n"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(sealed, "\n"), []byte("\n"))
	// Flip a character in the body, as a careless edit or a hostile one would.
	body := []byte(string(lines[3]))
	if body[0] == 'A' {
		body[0] = 'B'
	} else {
		body[0] = 'A'
	}
	lines[3] = body
	tampered := append(bytes.Join(lines, []byte("\n")), '\n')

	if _, err := Decrypt(tampered, "pass"); err == nil {
		t.Fatal("a tampered vault opened; the ciphertext is not authenticated")
	}
}

// TestTheHeaderIsAuthenticated. The header carries the KDF cost, so if it were
// not authenticated an attacker could edit 600000 down to 1 and test guesses
// almost for free — the parameters would be attacker-controlled input to the
// function that checks the attacker's guess.
func TestTheHeaderIsAuthenticated(t *testing.T) {
	sealed, err := Encrypt([]byte("DB_PASSWORD: hunter2\n"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	weakened := bytes.Replace(sealed, []byte(";600000"), []byte(";1"), 1)
	if bytes.Equal(weakened, sealed) {
		t.Fatal("the iteration count is not in the header, so this test proves nothing")
	}
	if _, err := Decrypt(weakened, "pass"); err == nil {
		t.Fatal("a vault with a weakened iteration count opened, so the header is not authenticated")
	}
}

// TestTwoEncryptionsOfOneSecretDiffer. A fresh salt and nonce each time, so
// committing the same value twice does not produce the same ciphertext — which
// would tell anyone reading the repository that the secret had not changed
// between two commits, or that two environments share one.
func TestTwoEncryptionsOfOneSecretDiffer(t *testing.T) {
	a, err := Encrypt([]byte("same"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt([]byte("same"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("encrypting the same plaintext twice produced identical files")
	}
}

// TestIsVaultRecognisesWithoutOpening. Configuration loading must be able to
// say "this file is encrypted" without the passphrase, rather than failing to
// parse it as YAML and reporting a syntax error.
func TestIsVaultRecognisesWithoutOpening(t *testing.T) {
	sealed, err := Encrypt([]byte("x: 1"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	if !IsVault(sealed) {
		t.Error("an encrypted file was not recognised as one")
	}
	if IsVault([]byte("DB_PASSWORD: hunter2\n")) {
		t.Error("a plain YAML file was mistaken for a vault")
	}
	if !IsVault(append([]byte("\n\n  "), sealed...)) {
		t.Error("leading whitespace hid the header")
	}
}

// TestAnUnknownCipherIsNamed rather than opened with the one this build has.
// Otherwise a file from a future version fails its authentication tag and the
// user is told their passphrase is wrong.
func TestAnUnknownCipherIsNamed(t *testing.T) {
	sealed, err := Encrypt([]byte("x: 1"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	future := bytes.Replace(sealed, []byte("AES256-GCM"), []byte("CHACHA20-POLY1305"), 1)
	_, err = Decrypt(future, "pass")
	if err == nil {
		t.Fatal("a vault naming an unimplemented cipher opened")
	}
	if strings.Contains(err.Error(), "passphrase") {
		t.Errorf("an unimplemented cipher was reported as a passphrase problem: %v", err)
	}
	if !strings.Contains(err.Error(), "CHACHA20-POLY1305") {
		t.Errorf("the error does not name what the file asked for: %v", err)
	}
}

// TestAFutureFormatVersionSaysSo, with advice that can be taken.
func TestAFutureFormatVersionSaysSo(t *testing.T) {
	sealed, err := Encrypt([]byte("x: 1"), "pass")
	if err != nil {
		t.Fatal(err)
	}
	future := bytes.Replace(sealed, []byte(Magic+";1;"), []byte(Magic+";99;"), 1)
	_, err = Decrypt(future, "pass")
	if err == nil {
		t.Fatal("a future format version opened")
	}
	if !strings.Contains(err.Error(), "newer infrena") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

// TestAnEmptyPassphraseIsRefused in both directions. An empty passphrase
// derives a perfectly valid key, so nothing downstream would complain — the
// file would simply be encrypted with a secret everybody knows.
func TestAnEmptyPassphraseIsRefused(t *testing.T) {
	if _, err := Encrypt([]byte("x: 1"), ""); err == nil {
		t.Error("encrypting with an empty passphrase was allowed")
	}
	sealed, _ := Encrypt([]byte("x: 1"), "pass")
	if _, err := Decrypt(sealed, ""); err == nil {
		t.Error("decrypting with an empty passphrase was allowed")
	}
}
