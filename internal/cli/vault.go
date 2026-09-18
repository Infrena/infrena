package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/vault"
)

// newVaultCommand builds `infrena vault`, which encrypts a file of secrets so
// it can live in version control beside the configuration that references it.
//
// A vault feeds ${secret.NAME}, the same reference the environment feeds. That
// is the point: configuration never says where a secret came from, so moving
// one between a vault and a pipeline's environment is not a configuration
// change and needs no review of the files that use it.
func newVaultCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Encrypt a file of secrets so it can be committed",
		Long: "Encrypt a file of secrets with a passphrase, so secrets can live in version " +
			"control beside the configuration that references them.\n\n" +
			"This is not a secret manager. It rotates nothing, controls nobody's access and " +
			"records no reads — it is a file you encrypt and commit, which is what you use when " +
			"you have no such store.\n\n" +
			"BEFORE YOU COMMIT ONE: ciphertext in git is permanent. If the repository is ever " +
			"published, every historical version of the file goes with it, and a passphrase " +
			"compromised later opens all of them. Rotate the secrets, not just the passphrase, " +
			"if that happens.",
	}
	cmd.AddCommand(
		newVaultCreateCommand(opts),
		newVaultEditCommand(opts),
		newVaultViewCommand(opts),
		newVaultEncryptCommand(opts),
		newVaultDecryptCommand(opts),
		newVaultRekeyCommand(opts),
	)
	return cmd
}

// vaultPassphrase resolves the passphrase or returns advice that can be acted
// on. Every subcommand goes through it, so they cannot drift about where a
// passphrase comes from.
func vaultPassphrase(opts *GlobalOptions) (string, error) {
	p, err := vault.Passphrase(opts.VaultPasswordFile)
	if errors.Is(err, vault.ErrNoPassphrase) {
		return "", errors.New(vault.NoPassphraseAdvice())
	}
	return p, err
}

func newVaultEncryptCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "encrypt <file>",
		Short:         "Encrypt a plain file in place",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := projectPath(opts, args[0])
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// REFUSED rather than nested. Encrypting twice produces a file that
			// needs two passphrases in the right order to open, and the second
			// layer hides that the first is there — so the mistake is invisible
			// until somebody cannot open a file they have the passphrase for.
			if vault.IsVault(data) {
				return fmt.Errorf("%s is already encrypted", path)
			}
			passphrase, err := vaultPassphrase(opts)
			if err != nil {
				return err
			}
			sealed, err := vault.Encrypt(data, passphrase)
			if err != nil {
				return err
			}
			if err := writeVault(path, sealed); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Encrypted %s\n", path)
			return nil
		},
	}
}

func newVaultDecryptCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "decrypt <file>",
		Short:         "Decrypt a vault file in place, leaving it readable",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := projectPath(opts, args[0])
			plaintext, err := openVault(opts, path)
			if err != nil {
				return err
			}
			// 0600 even though it is now plaintext the user asked for: this
			// file holds credentials and has just stopped being encrypted, so
			// the one protection left is who can read it.
			if err := os.WriteFile(path, plaintext, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Decrypted %s. It now holds secrets in clear — do not commit it.\n", path)
			return nil
		},
	}
}

func newVaultViewCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "view <file>",
		Short:         "Print a vault's contents without writing anything",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			plaintext, err := openVault(opts, projectPath(opts, args[0]))
			if err != nil {
				return err
			}
			// To stdout, unredacted, because printing it is the entire request.
			// Nothing here goes through report.Format: a `view` that redacted
			// what it was asked to show would be a command with no purpose.
			_, err = cmd.OutOrStdout().Write(plaintext)
			return err
		},
	}
}

func newVaultCreateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "create <file>",
		Short:         "Create a new vault and open it in $EDITOR",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := projectPath(opts, args[0])
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists; use `infrena vault edit %s`", path, args[0])
			}
			return editVault(cmd, opts, path, []byte(
				"# Secrets, encrypted when you close this editor.\n"+
					"# Each key is a name ${secret.NAME} can reference.\n"+
					"# DB_PASSWORD: change-me\n"))
		},
	}
}

func newVaultEditCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "edit <file>",
		Short:         "Decrypt a vault into $EDITOR and re-encrypt on save",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := projectPath(opts, args[0])
			plaintext, err := openVault(opts, path)
			if err != nil {
				return err
			}
			return editVault(cmd, opts, path, plaintext)
		},
	}
}

func newVaultRekeyCommand(opts *GlobalOptions) *cobra.Command {
	var newFile string
	cmd := &cobra.Command{
		Use:           "rekey <file>",
		Short:         "Re-encrypt a vault under a different passphrase",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if newFile == "" {
				return errors.New("rekey needs the new passphrase: pass --new-password-file")
			}
			path := projectPath(opts, args[0])
			plaintext, err := openVault(opts, path)
			if err != nil {
				return err
			}
			next, err := vault.Passphrase(newFile)
			if err != nil {
				return err
			}
			sealed, err := vault.Encrypt(plaintext, next)
			if err != nil {
				return err
			}
			if err := writeVault(path, sealed); err != nil {
				return err
			}
			// Said plainly because rekeying is often done in response to a leak,
			// and it is the wrong remedy on its own: the old ciphertext is still
			// in git history and the old passphrase still opens it.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Rekeyed %s.\nThe previous ciphertext is still in version control and the old "+
					"passphrase still opens it. If this was a leak, rotate the secrets themselves.\n",
				path)
			return nil
		},
	}
	cmd.Flags().StringVar(&newFile, "new-password-file", "",
		"file holding the new passphrase")
	return cmd
}

// projectPath resolves a vault file argument against the PROJECT directory,
// the way --var-file already resolves and the way secrets.yml is discovered.
//
// NOT AGAINST THE PROCESS DIRECTORY, and this is correctness rather than
// consistency. A vault is part of the project — it is committed beside the
// configuration that references it — and the loader finds it at
// <project>/secrets.yml. If the argument resolved against the shell's directory
// instead, `infrena --chdir infra vault edit secrets.yml` would edit
// ./secrets.yml while every plan read infra/secrets.yml, and the user would be
// editing a file nothing loads with no sign that anything was wrong.
//
// An absolute path is left alone: somebody who wrote one has answered the
// question.
func projectPath(opts *GlobalOptions, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(opts.Dir, p)
}

// openVault reads and decrypts, refusing a file that is not one.
func openVault(opts *GlobalOptions, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !vault.IsVault(data) {
		return nil, fmt.Errorf("%s is not an encrypted vault file", path)
	}
	passphrase, err := vaultPassphrase(opts)
	if err != nil {
		return nil, err
	}
	return vault.Decrypt(data, passphrase)
}

// writeVault writes a vault file at 0600.
//
// Encrypted content at 0600 looks like belt and braces and is not: the file's
// protection is the passphrase, and a passphrase is a thing people reuse. Being
// world-readable would hand an attacker offline guesses against it.
func writeVault(path string, sealed []byte) error {
	return os.WriteFile(path, sealed, 0o600)
}

// editVault runs $EDITOR over plaintext and re-encrypts the result.
//
// THE PLAINTEXT NEVER TOUCHES THE PROJECT DIRECTORY. It goes into a private
// temporary directory created 0700, so an editor's swap file, backup or crash
// recovery lands there too rather than beside the configuration where a `git
// add .` would sweep it up. The directory is removed on every path out,
// including the ones where the editor failed.
func editVault(cmd *cobra.Command, opts *GlobalOptions, path string, plaintext []byte) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		return errors.New("no $EDITOR set, so there is nothing to open the vault in.\n" +
			"Set EDITOR, or use `infrena vault view` and `infrena vault encrypt` instead")
	}

	dir, err := os.MkdirTemp("", "infrena-vault-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}

	tmp := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(tmp, plaintext, 0o600); err != nil {
		return err
	}

	// The editor gets the terminal: it is an interactive program and anything
	// else leaves the user looking at a frozen shell.
	ed := exec.Command("sh", "-c", editor+" "+shellQuote(tmp))
	ed.Stdin, ed.Stdout, ed.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := ed.Run(); err != nil {
		return fmt.Errorf("editor exited with an error, so %s is unchanged: %w", path, err)
	}

	edited, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	passphrase, err := vaultPassphrase(opts)
	if err != nil {
		return err
	}
	sealed, err := vault.Encrypt(edited, passphrase)
	if err != nil {
		return err
	}
	if err := writeVault(path, sealed); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", path)
	return nil
}

// shellQuote wraps a path for `sh -c`, so a temporary directory with a space or
// a quote in it cannot turn into extra arguments to the editor.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
