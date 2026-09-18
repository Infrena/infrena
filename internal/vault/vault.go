// Package vault encrypts a file of secrets with a passphrase, so that secrets
// can live in version control beside the configuration that references them.
//
// IT IS NOT A SECRET MANAGER, and the distinction is the open-core line
// (docs/open-core.md). A managed store rotates credentials, controls who may
// read which, and records who read what — those are things an organisation
// buys. This is a file you encrypt and commit, which is what a single operator
// or a CI runner uses when they have no such store. Having it here makes the
// paid product clearly about MANAGEMENT rather than about encryption.
//
// # The format
//
//	$INFRENA_VAULT;1;AES256-GCM;PBKDF2-SHA256;600000
//	<base64 salt>
//	<base64 nonce>
//	<base64 ciphertext>
//
// Self-describing on purpose. The header names the version, the cipher, the key
// derivation and its cost, so a file encrypted today still opens after any of
// those change — an encrypted file outlives the build that wrote it, which is
// the whole point of committing one.
//
// THE HEADER IS AUTHENTICATED. It is passed to GCM as additional data, so a
// file whose iteration count has been edited down fails to open rather than
// opening cheaply. Without that, the parameters would be attacker-controlled
// input to the function that checks the attacker's guess.
//
// # What this is not
//
// No invented cryptography: AES-256-GCM and PBKDF2-SHA256, both from the
// standard library, composed the ordinary way. GCM is an AEAD, so a tampered
// file fails to open rather than decrypting to something plausible.
//
// It still deserves review rather than trust. A passing test proves the round
// trip, which is the half that is easy to get right.
package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Magic opens every vault file. It is greppable on purpose: `grep -rl
	// '$INFRENA_VAULT'` finds every encrypted file in a repository, which is
	// what somebody auditing what is committed actually needs.
	Magic = "$INFRENA_VAULT"

	// FormatVersion is the envelope, not the cipher. It changes when the SHAPE
	// changes — a fourth line, a different encoding — never when a parameter
	// inside the header does, which is why the parameters are in the header.
	FormatVersion = 1

	cipherName = "AES256-GCM"
	kdfName    = "PBKDF2-SHA256"

	// iterations is the cost of a passphrase guess. OWASP's 2023 floor for
	// PBKDF2-SHA256 is 600,000, and this is deliberately AT that floor rather
	// than below it: the cost is paid once per command by one user, and the
	// attacker's cost is the whole point.
	//
	// Written into the header, so raising it later does not orphan a file
	// encrypted today.
	iterations = 600_000

	saltBytes = 16
	keyBytes  = 32 // AES-256
)

// ErrWrongPassphrase is returned when a file will not open. It does NOT
// distinguish a wrong passphrase from a corrupted or tampered file, and that is
// deliberate: GCM cannot tell them apart, and a message that claimed to would
// be guessing. The caller's advice has to cover both.
var ErrWrongPassphrase = errors.New("the vault would not open: wrong passphrase, or the file has been changed")

// IsVault reports whether data looks like an encrypted vault file.
//
// It reads the magic rather than trying to decrypt, so a caller can tell an
// encrypted file from a plain one without holding the passphrase — which is
// what `vault encrypt` needs in order to refuse double-encrypting, and what
// configuration loading needs in order to say "this file is encrypted" rather
// than failing to parse it as YAML.
func IsVault(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte(Magic))
}

// Encrypt seals plaintext under passphrase.
func Encrypt(plaintext []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("a vault passphrase is required")
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating a salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, keyBytes)
	if err != nil {
		return nil, fmt.Errorf("deriving a key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating a nonce: %w", err)
	}

	header := headerLine(iterations)
	// The header as additional data: it is authenticated but not encrypted, so
	// editing the iteration count breaks the open instead of weakening it.
	sealed := gcm.Seal(nil, nonce, plaintext, []byte(header))

	var out bytes.Buffer
	out.WriteString(header)
	out.WriteByte('\n')
	out.WriteString(base64.StdEncoding.EncodeToString(salt))
	out.WriteByte('\n')
	out.WriteString(base64.StdEncoding.EncodeToString(nonce))
	out.WriteByte('\n')
	out.WriteString(base64.StdEncoding.EncodeToString(sealed))
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Decrypt opens a vault file.
func Decrypt(data []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("a vault passphrase is required")
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		return nil, fmt.Errorf("this is not a vault file: expected %d lines, found %d", 4, len(lines))
	}
	header := strings.TrimRight(lines[0], "\r")
	iters, err := parseHeader(header)
	if err != nil {
		return nil, err
	}
	salt, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil {
		return nil, fmt.Errorf("the vault's salt is not valid base64: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[2]))
	if err != nil {
		return nil, fmt.Errorf("the vault's nonce is not valid base64: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil {
		return nil, fmt.Errorf("the vault's body is not valid base64: %w", err)
	}

	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iters, keyBytes)
	if err != nil {
		return nil, fmt.Errorf("deriving a key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("the vault's nonce is %d bytes, expected %d", len(nonce), gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, nonce, sealed, []byte(header))
	if err != nil {
		// Never wrapped with the underlying error: it says only
		// "cipher: message authentication failed", which tells a user
		// nothing they can act on and reads like a bug in infrena.
		return nil, ErrWrongPassphrase
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("preparing the cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("preparing the cipher: %w", err)
	}
	return gcm, nil
}

func headerLine(iters int) string {
	return fmt.Sprintf("%s;%d;%s;%s;%d", Magic, FormatVersion, cipherName, kdfName, iters)
}

// parseHeader reads the envelope and returns the iteration count it names.
//
// Every field is checked rather than skipped to the one that is needed. A file
// naming a cipher this build does not implement must say so, not be opened with
// the one it does implement and fail the authentication tag with a message
// about passphrases.
func parseHeader(header string) (int, error) {
	parts := strings.Split(header, ";")
	if len(parts) != 5 || parts[0] != Magic {
		return 0, fmt.Errorf("this is not a vault file: its first line is not a %s header", Magic)
	}
	version, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("the vault's format version %q is not a number", parts[1])
	}
	if version != FormatVersion {
		return 0, fmt.Errorf("this vault is format version %d and this build understands %d.\n"+
			"Use a newer infrena to open it", version, FormatVersion)
	}
	if parts[2] != cipherName || parts[3] != kdfName {
		return 0, fmt.Errorf("this vault uses %s with %s, and this build implements %s with %s",
			parts[2], parts[3], cipherName, kdfName)
	}
	iters, err := strconv.Atoi(parts[4])
	if err != nil || iters < 1 {
		return 0, fmt.Errorf("the vault's iteration count %q is not a positive number", parts[4])
	}
	return iters, nil
}
