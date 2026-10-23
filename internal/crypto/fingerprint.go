package crypto

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint names a credential without revealing it: the first 8 bytes of
// its SHA-256 in hex. Clients compute the same to tell whether a stored vendor
// key equals the one they hold, since the key itself is never returned.
func Fingerprint(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:8])
}
