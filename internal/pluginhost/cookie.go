package pluginhost

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// cookie is a fresh random value per launch.
//
// Its only job is to tell a plugin it was started by infrena rather than typed
// at a shell. It is not a security boundary — a plugin holds cloud credentials
// by design and the user chose to run it — so nothing verifies it beyond
// presence.
func cookie() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a plugin cookie: %w", err)
	}
	return hex.EncodeToString(b), nil
}
