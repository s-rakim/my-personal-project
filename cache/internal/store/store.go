// Package store holds cached content, addressed by the hash of the bytes
// themselves rather than by where they came from.
//
// Content addressing earns its place here for a reason specific to this setting.
// A school's uplink is the scarce resource, and the same bytes arrive under many
// different names: the same PDF linked from three places, the same video
// embedded in two lessons, the same package pulled by thirty machines from
// mirrors with different URLs. Keying on content rather than URL means each
// distinct byte sequence is stored and fetched exactly once, however many names
// point at it.
//
// It also makes writes safe for free: a file is named after its own hash, so a
// partially written one can never be mistaken for a complete one.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound means the digest is not in the store.
var ErrNotFound = errors.New("store: content not found")

// Digest is the lowercase hex SHA-256 of some content.
type Digest string

// Valid reports whether d looks like a SHA-256 digest. Checked before any digest
// reaches the filesystem, since it becomes part of a path.
func (d Digest) Valid() bool {
	if len(d) != 64 {
		return false
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Short returns a readable prefix, for logs.
func (d Digest) Short() string {
	if len(d) < 12 {
		return string(d)
	}
	return string(d[:12])
}

// Store is a content-addressed blob store on local disk.
type Store struct {
	root string

	mu    sync.RWMutex
	sizes map[Digest]int64
	bytes int64
}

// Open prepares a store rooted at dir, indexing whatever is already there.
//
// Scanning at startup rather than trusting a sidecar index is deliberate: the
// filesystem is the source of truth, so a crash, a manual deletion or a full
// disk cannot leave the index describing content that is not there.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", dir, err)
	}

	s := &Store{root: dir, sizes: make(map[Digest]int64)}
	if err := s.reindex(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) reindex() error {
	return filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, ".tmp") {
			return nil
		}
		digest := Digest(d.Name())
		if !digest.Valid() {
			// Not ours. Leave it alone rather than deleting something a human put
			// there.
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		s.sizes[digest] = info.Size()
		s.bytes += info.Size()
		return nil
	})
}

// path returns where a digest lives. Content is fanned out two levels so no
// single directory holds hundreds of thousands of entries, which some
// filesystems handle badly and every directory listing handles slowly.
func (s *Store) path(d Digest) string {
	return filepath.Join(s.root, string(d[:2]), string(d[2:4]), string(d))
}

// Has reports whether the store holds this content.
func (s *Store) Has(d Digest) bool {
	if !d.Valid() {
		return false
	}
	s.mu.RLock()
	_, ok := s.sizes[d]
	s.mu.RUnlock()
	return ok
}

// Size returns the stored size of some content.
func (s *Store) Size(d Digest) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.sizes[d]
	return n, ok
}

// Open returns a reader for stored content.
func (s *Store) Open(d Digest) (*os.File, error) {
	if !d.Valid() {
		return nil, fmt.Errorf("store: malformed digest %q", d)
	}
	f, err := os.Open(s.path(d))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", d.Short(), err)
	}
	return f, nil
}

// Put streams content in, returning its digest.
//
// The hash is computed while writing, so content is never trusted to be what a
// caller claimed. If the same bytes are already stored, the temporary file is
// discarded and the existing copy kept: that is the deduplication, and it is why
// the second school machine to request a package costs nothing.
func (s *Store) Put(r io.Reader) (Digest, int64, error) {
	tmp, err := os.CreateTemp(s.root, ".incoming-*.tmp")
	if err != nil {
		return "", 0, fmt.Errorf("store: create temporary file: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		cleanup()
		return "", 0, fmt.Errorf("store: write content: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("store: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", 0, fmt.Errorf("store: close: %w", err)
	}

	digest := Digest(hex.EncodeToString(hasher.Sum(nil)))

	if s.Has(digest) {
		os.Remove(tmpName)
		return digest, written, nil
	}

	final := s.path(digest)
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		os.Remove(tmpName)
		return "", 0, fmt.Errorf("store: create directory: %w", err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		os.Remove(tmpName)
		return "", 0, fmt.Errorf("store: chmod: %w", err)
	}
	// Rename is atomic within a filesystem, so readers see either nothing or
	// complete, verified content. There is no window in which a partial file is
	// served.
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", 0, fmt.Errorf("store: commit: %w", err)
	}

	s.mu.Lock()
	if _, exists := s.sizes[digest]; !exists {
		s.sizes[digest] = written
		s.bytes += written
	}
	s.mu.Unlock()

	return digest, written, nil
}

// Delete removes content. Deleting what is not there is not an error.
func (s *Store) Delete(d Digest) error {
	if !d.Valid() {
		return fmt.Errorf("store: malformed digest %q", d)
	}
	err := os.Remove(s.path(d))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: delete %s: %w", d.Short(), err)
	}

	s.mu.Lock()
	if n, ok := s.sizes[d]; ok {
		s.bytes -= n
		delete(s.sizes, d)
	}
	s.mu.Unlock()
	return nil
}

// Bytes returns the total size of stored content.
func (s *Store) Bytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bytes
}

// Count returns how many distinct blobs are stored.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sizes)
}

// Digests lists everything stored, in stable order.
func (s *Store) Digests() []Digest {
	s.mu.RLock()
	out := make([]Digest, 0, len(s.sizes))
	for d := range s.sizes {
		out = append(out, d)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
