package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"postra/internal/platform/crypto"
)

// Encrypted wraps a Local store with envelope encryption so that raw MIME
// originals and attachment bodies are ciphertext at rest (§14 데이터 저장 원칙,
// 보안 테스트 "Backup 유출 가정 → Key 없이 복호화 불가").
//
// Objects stay content-addressed by the PLAINTEXT SHA-256 (preserving dedup
// and the MIME-013 hash contract), while the bytes written to disk are the
// serialized crypto.Envelope. The AAD binds each object to its kind+hash so
// envelopes cannot be swapped between slots.
type Encrypted struct {
	inner *Local
	kek   *crypto.KEK
}

func NewEncrypted(inner *Local, kek *crypto.KEK) *Encrypted {
	return &Encrypted{inner: inner, kek: kek}
}

func objectAAD(kind, plaintextHash string) []byte {
	return []byte("postra-object:" + kind + ":" + plaintextHash)
}

// maxObjectBytes bounds in-memory buffering during encryption. Callers
// already enforce per-message size limits before reaching here.
const maxObjectBytes = 200 << 20

func (e *Encrypted) Put(kind string, r io.Reader) (string, string, int64, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxObjectBytes+1))
	if err != nil {
		return "", "", 0, err
	}
	if int64(len(data)) > maxObjectBytes {
		return "", "", 0, fmt.Errorf("object exceeds %d bytes", maxObjectBytes)
	}
	sum := sha256.Sum256(data)
	plaintextHash := hex.EncodeToString(sum[:])

	env, err := e.kek.Encrypt(data, objectAAD(kind, plaintextHash))
	if err != nil {
		return "", "", 0, err
	}
	blob, err := json.Marshal(env)
	if err != nil {
		return "", "", 0, err
	}
	u, err := e.inner.putBytes(kind, plaintextHash, blob)
	if err != nil {
		return "", "", 0, err
	}
	return u, plaintextHash, int64(len(data)), nil
}

func (e *Encrypted) Get(u string) (io.ReadCloser, error) {
	kind, plaintextHash, err := parseURI(u)
	if err != nil {
		return nil, err
	}
	rc, err := e.inner.Get(u)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	blob, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var env crypto.Envelope
	if err := json.Unmarshal(blob, &env); err != nil {
		return nil, fmt.Errorf("object %s is not an envelope: %w", u, err)
	}
	pt, err := e.kek.Decrypt(&env, objectAAD(kind, plaintextHash))
	if err != nil {
		return nil, fmt.Errorf("object %s decrypt failed: %w", u, err)
	}
	return io.NopCloser(bytes.NewReader(pt)), nil
}

var _ Store = (*Encrypted)(nil)
var _ Store = (*Local)(nil)
