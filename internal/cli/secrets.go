package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/infrena/infrena/internal/vault"
	"github.com/infrena/infrena/pkg/value"
)

const (
	// SecretsFileName is the vault every environment shares.
	SecretsFileName = "secrets.yml"
	// SecretsDirName holds a vault per environment, the same shape vars/ has
	// (§4.1): secrets/production.yml is production's, and it wins over the
	// shared file for any name both define.
	SecretsDirName = "secrets"
)

// secretSource builds the resolver for ${secret.NAME} (PLAN.md §36).
//
// PRECEDENCE, HIGHEST FIRST: the environment, then secrets/<environment>.yml,
// then secrets.yml.
//
// THE ENVIRONMENT WINS, which is the same direction the rest of the precedence
// ladder runs (§7: --var beats every file). It means CI can inject a rotated
// credential without editing and re-encrypting a committed file, and can run
// without the vault passphrase at all. The cost is real and worth stating: a
// leftover variable in somebody's shell silently shadows the committed one, and
// nothing warns about it — the same trade --var already makes.
//
// A VAULT IS OPENED ONLY IF ONE EXISTS AND ONLY ONCE. A project with no vault
// never asks for a passphrase, which is what keeps the feature invisible to
// everyone not using it; a project with one opens it lazily, so a command that
// references no secret needs no passphrase either.
func secretSource(opts *GlobalOptions, environment string) (func(string) (value.Value, bool), func() error) {
	var (
		loaded bool
		byName map[string]string
		failed error
	)
	resolve := func(name string) (value.Value, bool) {
		// The environment first, and before any decryption: a run that supplies
		// everything through the environment never needs the passphrase.
		if v, ok := secretsFromEnvironment(name); ok {
			return v, true
		}
		if !loaded {
			loaded = true
			byName, failed = loadVaults(opts, environment)
		}
		if failed != nil {
			// Reported through the diagnostic the evaluator raises for a
			// missing secret, which names the secret AND is attributed to the
			// configuration line that wanted it. Returning the error separately
			// would report a vault problem with no indication of what needed it.
			return value.Value{}, false
		}
		v, ok := byName[name]
		if !ok || v == "" {
			return value.Value{}, false
		}
		return value.String(v, value.SourceVariable), true
	}
	// The failure is reported through the SAME lazy load, so asking why costs
	// nothing extra and cannot disagree with what the resolver saw.
	return resolve, func() error { return failed }
}

// loadVaults reads every vault a project has, nearest last so it wins.
func loadVaults(opts *GlobalOptions, environment string) (map[string]string, error) {
	paths := []string{filepath.Join(opts.Dir, SecretsFileName)}
	if environment != "" {
		paths = append(paths, filepath.Join(opts.Dir, SecretsDirName, environment+".yml"))
	}

	out := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// A PLAIN FILE IS ACCEPTED, not refused. secrets.yml is where a person
		// keeps secrets before they encrypt it, and during `vault decrypt` it
		// is deliberately plaintext — refusing it would break the command that
		// produced it. What protects the user is that `vault encrypt` exists
		// and the documentation says to run it; what protects the secret is
		// that infrena never prints one either way.
		if vault.IsVault(data) {
			passphrase, perr := vaultPassphrase(opts)
			if perr != nil {
				return nil, fmt.Errorf("%s is encrypted. %w", path, perr)
			}
			data, err = vault.Decrypt(data, passphrase)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
		var entries map[string]string
		if err := yaml.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("%s is not a map of secret name to value: %w", path, err)
		}
		for k, v := range entries {
			out[k] = v
		}
	}
	return out, nil
}
