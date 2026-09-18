package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const vaultProject = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    password: ${secret.DB_PASSWORD}
`

// TestAVaultSuppliesASecret end to end: encrypt a file, commit-shaped, and
// plan against it with no secret in the environment at all.
func TestAVaultSuppliesASecret(t *testing.T) {
	dir := project(t, vaultProject)
	writeIn(t, dir, "secrets.yml", "DB_PASSWORD: from-the-vault\n")
	t.Setenv("INFRENA_VAULT_PASSWORD", "correct horse")

	if e := run(t, dir, "vault", "encrypt", "secrets.yml"); e.ExitCode != 0 {
		t.Fatalf("vault encrypt: %d\n%s", e.ExitCode, e.combined())
	}
	sealed, err := os.ReadFile(filepath.Join(dir, "secrets.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "from-the-vault") {
		t.Fatal("the encrypted file still contains the plaintext")
	}
	if !strings.HasPrefix(string(sealed), "$INFRENA_VAULT") {
		t.Errorf("the file is not greppable as a vault:\n%s", sealed)
	}

	os.Unsetenv("DB_PASSWORD")
	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 — the vault should have supplied the secret\n%s",
			r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), "from-the-vault") {
		t.Error("the secret was printed")
	}
	if !strings.Contains(r.combined(), "<sensitive>") {
		t.Errorf("a vault secret was not redacted:\n%s", r.combined())
	}
}

// TestTheEnvironmentBeatsTheVault. Same direction as the rest of the
// precedence ladder (--var beats files), so CI can inject a rotated credential
// without editing and re-encrypting a committed file.
func TestTheEnvironmentBeatsTheVault(t *testing.T) {
	dir := project(t, vaultProject)
	writeIn(t, dir, "secrets.yml", "DB_PASSWORD: from-the-vault\n")
	t.Setenv("INFRENA_VAULT_PASSWORD", "correct horse")
	if e := run(t, dir, "vault", "encrypt", "secrets.yml"); e.ExitCode != 0 {
		t.Fatalf("vault encrypt: %s", e.combined())
	}
	t.Setenv("DB_PASSWORD", "from-the-environment")

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	state, err := os.ReadFile(filepath.Join(dir, ".infra", "state", "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "from-the-environment") {
		t.Error("the vault won; the environment must take precedence")
	}
	if strings.Contains(string(state), "from-the-vault") {
		t.Error("the vault's value was used despite the environment supplying one")
	}
}

// TestAWrongPassphraseSaysSo rather than reporting the secret as unset.
//
// A vault that will not open reports EVERY name as missing, so without the
// source being able to explain itself the user is told "DB_PASSWORD is not set,
// set it in the environment" when their passphrase is simply wrong. Advice that
// confident and that wrong is worse than none.
func TestAWrongPassphraseSaysSo(t *testing.T) {
	dir := project(t, vaultProject)
	writeIn(t, dir, "secrets.yml", "DB_PASSWORD: from-the-vault\n")
	t.Setenv("INFRENA_VAULT_PASSWORD", "correct horse")
	if e := run(t, dir, "vault", "encrypt", "secrets.yml"); e.ExitCode != 0 {
		t.Fatalf("vault encrypt: %s", e.combined())
	}

	os.Unsetenv("DB_PASSWORD")
	t.Setenv("INFRENA_VAULT_PASSWORD", "wrong")
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("a wrong passphrase still produced a plan")
	}
	out := r.combined()
	if !strings.Contains(out, "wrong passphrase") {
		t.Errorf("the error does not name the likely cause:\n%s", out)
	}
	if strings.Contains(out, "Set DB_PASSWORD in the environment") {
		t.Errorf("a passphrase problem was reported as a missing environment variable:\n%s", out)
	}
}

// TestVaultRoundTripThroughTheCommands. view must not write, decrypt must
// restore exactly, and encrypt must refuse to nest.
func TestVaultRoundTripThroughTheCommands(t *testing.T) {
	dir := project(t, vaultProject)
	const plain = "DB_PASSWORD: hunter2\nAPI_KEY: abc123\n"
	writeIn(t, dir, "secrets.yml", plain)
	t.Setenv("INFRENA_VAULT_PASSWORD", "pass")

	run(t, dir, "vault", "encrypt", "secrets.yml")

	v := run(t, dir, "vault", "view", "secrets.yml")
	if v.ExitCode != 0 || v.Stdout != plain {
		t.Errorf("view did not print the original content: %d %q", v.ExitCode, v.Stdout)
	}
	// view must not have written anything back.
	after, _ := os.ReadFile(filepath.Join(dir, "secrets.yml"))
	if !strings.HasPrefix(string(after), "$INFRENA_VAULT") {
		t.Error("view decrypted the file on disk")
	}

	if again := run(t, dir, "vault", "encrypt", "secrets.yml"); again.ExitCode == 0 {
		t.Error("encrypting an encrypted file was allowed; it would need two passphrases to open")
	}

	if d := run(t, dir, "vault", "decrypt", "secrets.yml"); d.ExitCode != 0 {
		t.Fatalf("decrypt: %s", d.combined())
	}
	restored, _ := os.ReadFile(filepath.Join(dir, "secrets.yml"))
	if string(restored) != plain {
		t.Errorf("decrypt did not restore the original: %q", restored)
	}
}

// TestAProjectWithNoVaultNeedsNoPassphrase. The feature must be invisible to
// everyone not using it — including a project that supplies every secret
// through the environment.
func TestAProjectWithNoVaultNeedsNoPassphrase(t *testing.T) {
	dir := project(t, vaultProject)
	os.Unsetenv("INFRENA_VAULT_PASSWORD")
	t.Setenv("DB_PASSWORD", "from-the-environment")

	if r := run(t, dir, "plan", "dev"); r.ExitCode != 2 {
		t.Fatalf("a project with no vault asked for something: %d\n%s", r.ExitCode, r.combined())
	}
}
