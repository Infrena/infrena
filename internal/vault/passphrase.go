package vault

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// PasswordVar names the environment variable holding a vault passphrase.
const PasswordVar = "INFRENA_VAULT_PASSWORD"

// ErrNoPassphrase is returned when nothing supplied one.
var ErrNoPassphrase = errors.New("no vault passphrase")

// Passphrase resolves the passphrase, from the environment or a file.
//
// THERE IS NO INTERACTIVE PROMPT, and that is a deliberate v1 limitation
// rather than an oversight. Reading a passphrase without echoing it needs
// terminal control that the standard library does not expose, and this project
// holds a two-dependency budget (cobra and a YAML parser) that a terminal
// library would break. The alternative — prompting with the passphrase echoed
// onto the screen and into any scrollback — is worse than not prompting, so
// neither is offered.
//
// What remains covers both real cases. CI sets the environment variable, the
// way it sets every other secret. A person points --vault-password-file at a
// file outside the repository, which is also what makes `vault edit` scriptable
// and what stops a passphrase reaching shell history.
//
// A FILE RATHER THAN A FLAG holding the value, for the same reason: a
// --vault-password flag would put the passphrase in `ps` output and in the
// shell's history, where it outlives the command by a long way.
//
// The file's content is trimmed of trailing newlines only. Leading and interior
// whitespace is kept, because a passphrase may legitimately start with a space
// and silently discarding it would make a correct passphrase fail.
func Passphrase(passwordFile string) (string, error) {
	if passwordFile != "" {
		data, err := os.ReadFile(passwordFile)
		if err != nil {
			return "", fmt.Errorf("reading --vault-password-file: %w", err)
		}
		p := strings.TrimRight(string(data), "\r\n")
		if p == "" {
			return "", fmt.Errorf("--vault-password-file %s is empty", passwordFile)
		}
		return p, nil
	}
	if p := os.Getenv(PasswordVar); p != "" {
		return p, nil
	}
	return "", ErrNoPassphrase
}

// NoPassphraseAdvice is the message a command shows when nothing supplied one.
// One text, so every command that needs a passphrase asks for it the same way.
func NoPassphraseAdvice() string {
	return "No vault passphrase.\n" +
		"Set " + PasswordVar + ", or pass --vault-password-file pointing at a file that holds it.\n" +
		"There is no prompt: reading one without echoing it needs a terminal library this " +
		"project does not depend on, and echoing a passphrase is worse than not asking."
}
