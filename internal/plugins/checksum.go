package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// checksumPrefix names the algorithm, and is part of the recorded string rather
// than decoration: plugins.lock is committed and may be read by a later build
// that has learned a second hash, and a bare hex digest says nothing about what
// produced it.
const checksumPrefix = "sha256:"

// Sum is the checksum of some bytes, in the form the lockfile records.
func Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return checksumPrefix + hex.EncodeToString(sum[:])
}

// FileChecksum is the checksum of a file on disk, in the form the lockfile
// records.
//
// Streamed rather than read whole: the loader calls this before launching every
// plugin, and a plugin binary is several megabytes.
func FileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("reading %s to check it against %s: %w", path, LockfileName, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s to check it against %s: %w", path, LockfileName, err)
	}
	return checksumPrefix + hex.EncodeToString(h.Sum(nil)), nil
}
