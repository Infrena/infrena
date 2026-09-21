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
	// SecretsDirName is the directory holding a vault per environment, the same
	// shape vars/ has: secrets/production.yml is production's, and it wins
	// over the shared file for any name both define.
	SecretsDirName = "secrets"
)

// secretSource builds the resolver for ${secret.NAME}.
//
// Precedence, highest first: the process environment, then
// secrets/<environment>.yml, then secrets.yml. The environment winning lets CI
// inject a rotated credential without re-encrypting a committed file, and run
// without the vault passphrase at all. The cost, as with --var, is that a
// leftover variable in somebody's shell silently shadows the committed one.
//
// A vault is opened only if one exists, and only once. A project with no vault
// is never asked for a passphrase, and a command that references no secret is
// not asked either.
func secretSource(opts *GlobalOptions, environment string) (func(string) (value.Value, bool), func() error) {
	var (
		loaded bool
		byName map[string]string
		failed error
	)
	resolve := func(name string) (value.Value, bool) {
		// The environment first, and before any decryption, so a run that
		// supplies everything through it never needs the passphrase.
		if v, ok := secretsFromEnvironment(name); ok {
			return v, true
		}
		if !loaded {
			loaded = true
			byName, failed = loadVaults(opts, environment)
		}
		if failed != nil {
			// Reported through the evaluator's missing-secret diagnostic,
			// which names the secret and the configuration line that wanted
			// it. Returning the error here would report a vault problem with
			// no indication of what needed the vault.
			return value.Value{}, false
		}
		v, ok := byName[name]
		if !ok || v == "" {
			return value.Value{}, false
		}
		return value.String(v, value.SourceVariable), true
	}
	// The failure comes from the same lazy load, so it cannot disagree with
	// what the resolver saw.
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
		// A plain file is accepted rather than refused: secrets.yml is
		// plaintext before it is encrypted and again after `vault decrypt`, so
		// refusing it would break the command that produced it.
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
