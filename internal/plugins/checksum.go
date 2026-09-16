package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// checksumPrefix names the algorithm in the recorded value.
//
// IT IS PART OF THE RECORDED STRING, not decoration. plugins.lock is committed
// and read years later, possibly by a build that has learned a second hash, and
// a bare hex digest says nothing about what produced it. A prefix makes the
// upgrade a comparison the reader can see rather than a silent reinterpretation
// of sixty-four characters.
const checksumPrefix = "sha256:"

// Sum is the checksum of some bytes, in the form the lockfile records.
func Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return checksumPrefix + hex.EncodeToString(sum[:])
}

// FileChecksum is the checksum of a file on disk, in the form the lockfile
// records.
//
// STREAMED rather than read whole. This runs on the hot path - the loader
// checks a binary against the lock before launching it - and a plugin is
// several megabytes, so reading one into memory to hash it would cost the
// memory for no reason.
//
// OFFLINE, like everything else in this package. It hashes a path.
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
