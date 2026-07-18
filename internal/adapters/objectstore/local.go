// Package objectstore stores raw RFC822 MIME originals and attachment
// bodies outside the DB (MIME-001/014). The local backend is
// content-addressed by SHA-256; an S3-compatible backend can implement the
// same interface later.
package objectstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Store interface {
	// Put stores content and returns (uri, sha256hex, size).
	Put(kind string, r io.Reader) (string, string, int64, error)
	Get(uri string) (io.ReadCloser, error)
	// Delete removes an object. Missing objects are not an error.
	Delete(uri string) error
}

type Local struct {
	root string
}

func NewLocal(dataDir string) (*Local, error) {
	root := filepath.Join(dataDir, "objects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Local{root: root}, nil
}

func (l *Local) Put(kind string, r io.Reader) (string, string, int64, error) {
	tmp, err := os.CreateTemp(l.root, "ingest-*")
	if err != nil {
		return "", "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	tmp.Close()
	if err != nil {
		return "", "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := l.commit(kind, sum, tmp.Name()); err != nil {
		return "", "", 0, err
	}
	return uri(kind, sum), sum, n, nil
}

// putBytes stores data under a caller-chosen content name (used by the
// encrypting wrapper, which addresses objects by the PLAINTEXT hash while
// the bytes on disk are ciphertext).
func (l *Local) putBytes(kind, name string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(l.root, "ingest-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if err := l.commit(kind, name, tmp.Name()); err != nil {
		return "", err
	}
	return uri(kind, name), nil
}

func (l *Local) commit(kind, name, tmpPath string) error {
	dir := filepath.Join(l.root, kind, name[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		if err := os.Rename(tmpPath, dst); err != nil {
			return err
		}
		os.Chmod(dst, 0o600)
	}
	return nil
}

func (l *Local) Get(u string) (io.ReadCloser, error) {
	kind, name, err := parseURI(u)
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- kind/name are validated by parseURI; path is scoped under the app-owned store root.
	return os.Open(filepath.Join(l.root, kind, name[:2], name))
}

func (l *Local) Delete(u string) error {
	kind, name, err := parseURI(u)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(l.root, kind, name[:2], name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Walk visits every stored object as (kind, contentName, rawBytes). Used by
// the encrypting wrapper to rewrap envelopes during KEK rotation.
func (l *Local) Walk(fn func(kind, name string, blob []byte) error) error {
	return filepath.WalkDir(l.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 { // kind/<prefix>/<name>
			return nil
		}
		kind, name := parts[0], parts[2]
		blob, err := os.ReadFile(p) // #nosec G122 G304 -- walking the app-owned store root; p is derived from that root, not user input
		if err != nil {
			return err
		}
		return fn(kind, name, blob)
	})
}

// Overwrite replaces an existing object's bytes in place (same content path).
func (l *Local) Overwrite(kind, name string, data []byte) error {
	dst := filepath.Join(l.root, kind, name[:2], name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func uri(kind, name string) string { return fmt.Sprintf("local://%s/%s", kind, name) }

func parseURI(u string) (kind, name string, err error) {
	rest, ok := strings.CutPrefix(u, "local://")
	if !ok {
		return "", "", fmt.Errorf("unsupported object URI %q", u)
	}
	kind, name, ok = strings.Cut(rest, "/")
	if !ok || len(name) < 2 || strings.ContainsAny(kind, `/\.`) || strings.ContainsAny(name, `/\.`) {
		return "", "", fmt.Errorf("malformed object URI %q", u)
	}
	return kind, name, nil
}
