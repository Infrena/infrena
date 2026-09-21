package vault

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAPasswordFileBeatsTheEnvironment. An explicit flag is the user answering
// the question for this command; the environment answers it for every command.
func TestAPasswordFileBeatsTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PasswordVar, "from-the-environment")

	got, err := Passphrase(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-the-file" {
		t.Errorf("Passphrase = %q, want the file's content", got)
	}
}

// TestTrailingNewlinesAreTrimmedAndNothingElseIs.
//
// Every editor adds a trailing newline, so a passphrase file would otherwise
// almost never work. Leading and interior whitespace is KEPT: a passphrase may
// legitimately begin with a space, and silently discarding it would make a
// correct passphrase fail with a message about it being wrong.
func TestTrailingNewlinesAreTrimmedAndNothingElseIs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("  pass phrase  \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Passphrase(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "  pass phrase  " {
		t.Errorf("Passphrase = %q, want the leading and interior spaces kept", got)
	}
}

// TestAnEmptyPasswordFileIsRefused rather than treated as an empty passphrase,
// which would encrypt under a secret everybody knows.
func TestAnEmptyPasswordFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Passphrase(path); err == nil {
		t.Error("an empty password file was accepted")
	}
}

// TestNoSourceAndNoTerminalIsRefusedNotHung. Under `go test` stdin is not a
// terminal, which is the same condition CI has: the run must fail with advice
// rather than block on a prompt nobody can answer.
func TestNoSourceAndNoTerminalIsRefusedNotHung(t *testing.T) {
	t.Setenv(PasswordVar, "")
	if _, err := Passphrase(""); err != ErrNoPassphrase {
		t.Errorf("err = %v, want ErrNoPassphrase — a non-interactive run must not prompt", err)
	}
	if _, err := NewPassphrase(""); err != ErrNoPassphrase {
		t.Errorf("NewPassphrase err = %v, want ErrNoPassphrase", err)
	}
	if Interactive() {
		t.Error("Interactive() is true under `go test`, where stdin is not a terminal")
	}
}

// TestANonInteractiveNewPassphraseIsNotConfirmed. A file holds what it holds;
// asking twice would mean reading the same bytes twice.
func TestANonInteractiveNewPassphraseIsNotConfirmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("new-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewPassphrase(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-pass" {
		t.Errorf("NewPassphrase = %q", got)
	}
}
