// Package vault seals third-party credentials (OAuth refresh tokens, API keys) before they are stored, using AES-256-GCM.
//
// What it guarantees, and what it does not:
//
//   - A stored secret is useless without the master key, which lives outside the database (an environment variable or a
//     secret manager), so a database dump or backup does not expose credentials.
//   - Each secret is bound to its tenant and name through the AEAD's associated data. Copying a ciphertext into another
//     tenant's row, or under another name, makes decryption FAIL, so a bug or an attacker with write access to the table
//     cannot make tenant B use tenant A's token.
//   - Keys are versioned. New secrets use the current key; old secrets stay readable under the key that sealed them, so a
//     key can be rotated by adding a new one, re-sealing secrets over time, and retiring the old one.
//   - A nonce is generated randomly for every seal; with a 96-bit random nonce this is safe for far more secrets than
//     one business will ever store under a key.
//
// It does NOT protect against someone who has the master key AND database access, nor against a compromised
// application process (which holds decrypted secrets in memory while it uses them). Rotate keys if the master key leaks.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// MaxSecretBytes bounds a plaintext secret.
const MaxSecretBytes = 64 << 10

// ErrUnreadable is returned when a secret cannot be opened: wrong key, wrong tenant or name, or tampering. It carries
// no detail on purpose.
var ErrUnreadable = errors.New("vault: secret cannot be decrypted")

// Sealed is a stored secret. It contains no plaintext.
type Sealed struct {
	KeyVersion int
	Nonce      []byte
	Ciphertext []byte
}

// Vault seals and opens secrets. Safe for concurrent use.
type Vault struct {
	keys    map[int]cipher.AEAD
	current int
}

// New builds a vault from versioned 32-byte keys; current names the key used for new secrets.
func New(keys map[int][]byte, current int) (*Vault, error) {
	if len(keys) == 0 {
		return nil, errors.New("vault: at least one key is required")
	}
	v := &Vault{keys: map[int]cipher.AEAD{}, current: current}
	for ver, k := range keys {
		if ver <= 0 {
			return nil, errors.New("vault: key versions start at 1")
		}
		if len(k) != 32 {
			return nil, fmt.Errorf("vault: key %d must be 32 bytes (AES-256), got %d", ver, len(k))
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		if v.keys[ver], err = cipher.NewGCM(block); err != nil {
			return nil, err
		}
	}
	if _, ok := v.keys[current]; !ok {
		return nil, fmt.Errorf("vault: the current key version %d is not among the keys", current)
	}
	return v, nil
}

// FromEnv builds a vault from VENTURE_SECRETS_KEYS ("1:<base64 key>,2:<base64 key>") and VENTURE_SECRETS_CURRENT.
// Generate a key with: openssl rand -base64 32
func FromEnv(get func(string) string) (*Vault, error) {
	raw := strings.TrimSpace(get("VENTURE_SECRETS_KEYS"))
	if raw == "" {
		return nil, errors.New("vault: VENTURE_SECRETS_KEYS is not set")
	}
	keys := map[int][]byte{}
	for _, part := range strings.Split(raw, ",") {
		ver, b64, ok := strings.Cut(strings.TrimSpace(part), ":")
		n, err := strconv.Atoi(ver)
		if !ok || err != nil {
			return nil, errors.New("vault: VENTURE_SECRETS_KEYS must look like 1:<base64>,2:<base64>")
		}
		k, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("vault: key %d is not valid base64", n)
		}
		keys[n] = k
	}
	current := 0
	if c := strings.TrimSpace(get("VENTURE_SECRETS_CURRENT")); c != "" {
		var err error
		if current, err = strconv.Atoi(c); err != nil {
			return nil, errors.New("vault: VENTURE_SECRETS_CURRENT must be a number")
		}
	} else {
		vers := make([]int, 0, len(keys))
		for k := range keys {
			vers = append(vers, k)
		}
		sort.Ints(vers)
		current = vers[len(vers)-1]
	}
	return New(keys, current)
}

func aad(tenant uuid.UUID, name string) []byte {
	return []byte("venture-os/secret/v1|" + tenant.String() + "|" + name)
}

// Seal encrypts plaintext for (tenant, name).
func (v *Vault) Seal(tenant uuid.UUID, name string, plaintext []byte) (Sealed, error) {
	if tenant == uuid.Nil || name == "" {
		return Sealed{}, errors.New("vault: a tenant and a name are required")
	}
	if len(plaintext) == 0 || len(plaintext) > MaxSecretBytes {
		return Sealed{}, fmt.Errorf("vault: a secret must be 1 to %d bytes", MaxSecretBytes)
	}
	g := v.keys[v.current]
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyVersion: v.current, Nonce: nonce, Ciphertext: g.Seal(nil, nonce, plaintext, aad(tenant, name))}, nil
}

// Open decrypts a secret sealed for (tenant, name).
func (v *Vault) Open(tenant uuid.UUID, name string, s Sealed) ([]byte, error) {
	g, ok := v.keys[s.KeyVersion]
	if !ok || len(s.Nonce) != g.NonceSize() {
		return nil, ErrUnreadable
	}
	pt, err := g.Open(nil, s.Nonce, s.Ciphertext, aad(tenant, name))
	if err != nil {
		return nil, ErrUnreadable
	}
	return pt, nil
}

// NeedsRotation reports whether a secret was sealed under a key other than the current one.
func (v *Vault) NeedsRotation(s Sealed) bool { return s.KeyVersion != v.current }
