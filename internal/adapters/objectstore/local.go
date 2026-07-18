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
	dir := filepath.Join(l.root, kind, sum[:2])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", 0, err
	}
	dst := filepath.Join(dir, sum)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return "", "", 0, err
		}
		os.Chmod(dst, 0o600)
	}
	return fmt.Sprintf("local://%s/%s", kind, sum), sum, n, nil
}

func (l *Local) Get(uri string) (io.ReadCloser, error) {
	rest, ok := strings.CutPrefix(uri, "local://")
	if !ok {
		return nil, fmt.Errorf("unsupported object URI %q", uri)
	}
	kind, sum, ok := strings.Cut(rest, "/")
	if !ok || strings.ContainsAny(kind, `/\.`) || strings.ContainsAny(sum, `/\.`) {
		return nil, fmt.Errorf("malformed object URI %q", uri)
	}
	return os.Open(filepath.Join(l.root, kind, sum[:2], sum))
}
