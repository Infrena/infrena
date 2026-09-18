package vault

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// PasswordVar names the environment variable holding a vault passphrase.
const PasswordVar = "INFRENA_VAULT_PASSWORD"

// ErrNoPassphrase is returned when nothing supplied one and nobody could be asked.
var ErrNoPassphrase = errors.New("no vault passphrase")

// Passphrase resolves the passphrase for opening an existing vault.
//
// THREE SOURCES, HIGHEST FIRST: --vault-password-file, the environment, then a
// prompt on the terminal. The order is deliberate — an explicit flag is the
// user answering the question for this command, the environment is how CI
// answers it for every command, and the prompt is the fallback for a person who
// has answered it nowhere.
//
// A FILE RATHER THAN A FLAG HOLDING THE VALUE. There is no --vault-password,
// because a passphrase in an argument is in `ps` output for the life of the
// command and in shell history for much longer.
//
// The prompt reads without echo through golang.org/x/term, which is the third
// dependency this project takes (README, CONTRIBUTING). It was added
// deliberately, at the owner's direction, after shipping without one: the
// alternatives were echoing a passphrase onto the screen and into scrollback,
// or requiring a password file for every local `vault edit`. Neither is worth
// saving a dependency the Go team maintains.
//
// The file's content is trimmed of trailing newlines only. Leading and interior
// whitespace is kept, because a passphrase may legitimately begin with a space
// and silently discarding it would make a correct one fail.
func Passphrase(passwordFile string) (string, error) {
	if passwordFile != "" {
		return passphraseFromFile(passwordFile)
	}
	if p := os.Getenv(PasswordVar); p != "" {
		return p, nil
	}
	if !Interactive() {
		return "", ErrNoPassphrase
	}
	return prompt("Vault passphrase: ")
}

// NewPassphrase resolves the passphrase for a vault being created or rekeyed,
// asking TWICE when it prompts.
//
// The confirmation exists because the mistake is not symmetric. A typo when
// OPENING a vault costs one failed command; a typo when SEALING one produces a
// file nobody can open, encrypted under a passphrase that exists only in the
// typo — and it is usually discovered later, by somebody else, with the
// original plaintext long gone.
//
// Non-interactive sources are not confirmed: a file or an environment variable
// holds what it holds, and asking twice would mean reading the same bytes
// twice.
func NewPassphrase(passwordFile string) (string, error) {
	if passwordFile != "" {
		return passphraseFromFile(passwordFile)
	}
	if p := os.Getenv(PasswordVar); p != "" {
		return p, nil
	}
	if !Interactive() {
		return "", ErrNoPassphrase
	}
	first, err := prompt("New vault passphrase: ")
	if err != nil {
		return "", err
	}
	again, err := prompt("Confirm passphrase: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the two passphrases do not match, so nothing was written.\n" +
			"A mistyped passphrase here produces a file nobody can open")
	}
	return first, nil
}

// Interactive reports whether there is a terminal to ask.
//
// Checked on STDIN, which is what would be read. A run with stdin redirected
// from a pipe has nobody to ask even when stdout is still a terminal, and
// prompting there would block a pipeline forever on input that is not coming.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func passphraseFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading --vault-password-file: %w", err)
	}
	p := strings.TrimRight(string(data), "\r\n")
	if p == "" {
		return "", fmt.Errorf("--vault-password-file %s is empty", path)
	}
	return p, nil
}

// prompt reads one passphrase without echoing it.
//
// The PROMPT GOES TO STDERR, never stdout: `infrena vault view secrets.yml >
// plain.yml` must write the vault's contents to that file and the question to
// the terminal, and a prompt on stdout would end up in the file.
//
// The newline after reading is ours to print. ReadPassword consumes the
// keystroke that ended the line without echoing it, so without this the next
// output starts on the same line as the prompt.
func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the passphrase: %w", err)
	}
	p := strings.TrimRight(string(line), "\r\n")
	if p == "" {
		return "", errors.New("an empty passphrase is refused")
	}
	return p, nil
}

// NoPassphraseAdvice is the message a command shows when nothing supplied one
// and there was no terminal to ask. One text, so every command asks alike.
func NoPassphraseAdvice() string {
	return "No vault passphrase, and no terminal to ask on.\n" +
		"Set " + PasswordVar + ", or pass --vault-password-file pointing at a file that holds it.\n" +
		"Run this from a terminal to be prompted instead."
}
