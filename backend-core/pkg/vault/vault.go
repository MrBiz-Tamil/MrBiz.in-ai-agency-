// Package vault encrypts third-party credentials at rest (AES-256-GCM).
//
// Each ciphertext is bound to its workspace through the AEAD additional data, so a secret
// copied from one workspace's row into another's cannot be decrypted there.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const prefix = "v1:"

var (
	ErrKeySize    = errors.New("vault key must be 32 bytes (base64-encoded)")
	ErrBadPayload = errors.New("vault payload is malformed or cannot be decrypted")
)

type Vault struct{ aead cipher.AEAD }

// New builds a vault from a base64-encoded 32-byte key.
func New(keyB64 string) (*Vault, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil || len(key) != 32 {
		return nil, ErrKeySize
	}
	return fromKey(key)
}

// NewRandom creates a vault with a fresh random key (development only: data does not survive restarts).
func NewRandom() (*Vault, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return fromKey(key)
}

func fromKey(key []byte) (*Vault, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// GenerateKey returns a new random key in the format New expects.
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func (v *Vault) Encrypt(workspaceID string, plaintext []byte) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := v.aead.Seal(nil, nonce, plaintext, []byte(workspaceID))
	return prefix + base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

func (v *Vault) Decrypt(workspaceID, payload string) ([]byte, error) {
	if !strings.HasPrefix(payload, prefix) {
		return nil, ErrBadPayload
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(payload, prefix))
	if err != nil || len(raw) < v.aead.NonceSize()+v.aead.Overhead() {
		return nil, ErrBadPayload
	}
	n := v.aead.NonceSize()
	pt, err := v.aead.Open(nil, raw[:n], raw[n:], []byte(workspaceID))
	if err != nil {
		return nil, ErrBadPayload
	}
	return pt, nil
}
