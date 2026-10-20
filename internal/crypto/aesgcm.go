// Package crypto encrypts vendor LLM keys at rest with AES-256-GCM.
//
// The keyring is parsed from KTMS_ENCRYPTION_KEYS ("1:<base64 32B>,2:...").
// Every ciphertext records the key version that sealed it, so rotation is:
// add a higher version to the env, redeploy, re-encrypt all rows, drop the old
// version. Direct GCM rather than envelope encryption — this guards a few dozen
// rows, and a KMS round trip per read would buy nothing.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const keySize = 32

var (
	ErrNoKeys         = errors.New("crypto: no encryption keys configured")
	ErrUnknownVersion = errors.New("crypto: unknown key version")
)

// Keyring holds every key version the service can decrypt with, plus the
// active version used for new writes.
type Keyring struct {
	keys   map[int]cipher.AEAD
	active int
}

// ParseKeyring reads "version:base64key[,version:base64key]". The highest
// version becomes active. Versions must be positive and keys exactly 32 bytes.
func ParseKeyring(spec string) (*Keyring, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, ErrNoKeys
	}

	kr := &Keyring{keys: map[int]cipher.AEAD{}}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		version, key, err := parseEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, dup := kr.keys[version]; dup {
			return nil, fmt.Errorf("crypto: duplicate key version %d", version)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("crypto: version %d: %w", version, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("crypto: version %d: %w", version, err)
		}
		kr.keys[version] = aead
		if version > kr.active {
			kr.active = version
		}
	}
	if len(kr.keys) == 0 {
		return nil, ErrNoKeys
	}
	return kr, nil
}

func parseEntry(entry string) (int, []byte, error) {
	colon := strings.Index(entry, ":")
	if colon <= 0 {
		return 0, nil, fmt.Errorf("crypto: malformed key entry, want <version>:<base64key>")
	}
	version, err := strconv.Atoi(entry[:colon])
	if err != nil || version <= 0 {
		return 0, nil, fmt.Errorf("crypto: invalid key version %q", entry[:colon])
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(entry[colon+1:]))
	if err != nil {
		return 0, nil, fmt.Errorf("crypto: version %d: invalid base64: %w", version, err)
	}
	if len(key) != keySize {
		return 0, nil, fmt.Errorf("crypto: version %d: key must be %d bytes, got %d", version, keySize, len(key))
	}
	return version, key, nil
}

// ActiveVersion is the key version Encrypt seals with.
func (k *Keyring) ActiveVersion() int { return k.active }

// Versions lists every version the keyring can decrypt, unordered.
func (k *Keyring) Versions() []int {
	out := make([]int, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, v)
	}
	return out
}

// Encrypt seals plaintext with the active key. The returned blob is
// nonce||ciphertext and is stored as-is alongside the returned version.
func (k *Keyring) Encrypt(plaintext string) ([]byte, int, error) {
	aead, ok := k.keys[k.active]
	if !ok {
		return nil, 0, ErrNoKeys
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, fmt.Errorf("crypto: nonce: %w", err)
	}
	// Seal appends to nonce so the blob is self-contained.
	return aead.Seal(nonce, nonce, []byte(plaintext), nil), k.active, nil
}

// Decrypt opens a nonce||ciphertext blob sealed with the given key version.
func (k *Keyring) Decrypt(blob []byte, version int) (string, error) {
	aead, ok := k.keys[version]
	if !ok {
		return "", fmt.Errorf("%w: %d", ErrUnknownVersion, version)
	}
	if len(blob) < aead.NonceSize() {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, sealed := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: open: %w", err)
	}
	return string(plaintext), nil
}

// EncodeVersioned encodes a version and ciphertext blob as "v:<base64>".
func EncodeVersioned(blob []byte, version int) string {
	return fmt.Sprintf("%d:%s", version, base64.StdEncoding.EncodeToString(blob))
}

// DecodeVersioned decodes an encoded string and decrypts it with the keyring.
func (k *Keyring) DecodeVersioned(encoded string) (string, error) {
	colon := strings.Index(encoded, ":")
	if colon <= 0 {
		return "", errors.New("crypto: invalid encoded format")
	}
	v, err := strconv.Atoi(encoded[:colon])
	if err != nil {
		return "", fmt.Errorf("crypto: invalid version in encoded string: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(encoded[colon+1:])
	if err != nil {
		return "", fmt.Errorf("crypto: invalid base64 in encoded string: %w", err)
	}
	return k.Decrypt(blob, v)
}
