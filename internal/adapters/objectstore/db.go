package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// RawBackend is the low-level content-addressed blob store that the Encrypted
// wrapper builds on. Both the on-disk Local backend and the DB-backed DBStore
// implement it. It includes an unexported method, so only backends inside this
// package can satisfy it.
type RawBackend interface {
	Store
	putBytes(kind, name string, data []byte) (string, error)
	Walk(fn func(kind, name string, blob []byte) error) error
	Overwrite(kind, name string, data []byte) error
}

// BlobStore is the persistence port used by DBStore: content-addressed blob
// rows in the shared application database. Both the SQLite and PostgreSQL
// adapters implement it. Storing raw MIME originals and attachment bodies here
// (instead of a per-node local directory) lets them survive a pod restart and
// be readable from every replica.
type BlobStore interface {
	PutObject(ctx context.Context, kind, name string, blob []byte) error
	GetObject(ctx context.Context, kind, name string) ([]byte, error)
	DeleteObject(ctx context.Context, kind, name string) error
	WalkObjects(ctx context.Context, fn func(kind, name string, blob []byte) error) error
	OverwriteObject(ctx context.Context, kind, name string, blob []byte) error
}

// DBStore keeps objects in the shared database. When a fallback Local store is
// provided, reads that miss in the DB fall through to it, so objects written by
// an older on-disk deployment stay readable across the upgrade.
type DBStore struct {
	blobs    BlobStore
	fallback *Local
}

func NewDB(blobs BlobStore, fallback *Local) *DBStore {
	return &DBStore{blobs: blobs, fallback: fallback}
}

func (d *DBStore) Put(kind string, r io.Reader) (string, string, int64, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxObjectBytes+1))
	if err != nil {
		return "", "", 0, err
	}
	if int64(len(data)) > maxObjectBytes {
		return "", "", 0, fmt.Errorf("object exceeds %d bytes", maxObjectBytes)
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	if err := d.blobs.PutObject(context.Background(), kind, name, data); err != nil {
		return "", "", 0, err
	}
	return uri(kind, name), name, int64(len(data)), nil
}

func (d *DBStore) putBytes(kind, name string, data []byte) (string, error) {
	if err := d.blobs.PutObject(context.Background(), kind, name, data); err != nil {
		return "", err
	}
	return uri(kind, name), nil
}

func (d *DBStore) Get(u string) (io.ReadCloser, error) {
	kind, name, err := parseURI(u)
	if err != nil {
		return nil, err
	}
	blob, err := d.blobs.GetObject(context.Background(), kind, name)
	if err != nil {
		if d.fallback != nil {
			// Legacy object written to local disk before the DB migration.
			return d.fallback.Get(u)
		}
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(blob)), nil
}

func (d *DBStore) Delete(u string) error {
	kind, name, err := parseURI(u)
	if err != nil {
		return err
	}
	if d.fallback != nil {
		_ = d.fallback.Delete(u)
	}
	return d.blobs.DeleteObject(context.Background(), kind, name)
}

func (d *DBStore) Walk(fn func(kind, name string, blob []byte) error) error {
	return d.blobs.WalkObjects(context.Background(), fn)
}

func (d *DBStore) Overwrite(kind, name string, data []byte) error {
	return d.blobs.OverwriteObject(context.Background(), kind, name, data)
}

var _ RawBackend = (*DBStore)(nil)
var _ RawBackend = (*Local)(nil)
