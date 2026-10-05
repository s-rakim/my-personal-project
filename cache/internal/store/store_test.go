package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPutDeduplicatesIdenticalContent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	body := strings.Repeat("the same bytes", 500)
	d1, n1, err := s.Put(strings.NewReader(body))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	d2, n2, err := s.Put(strings.NewReader(body))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if d1 != d2 {
		t.Errorf("identical content produced different digests: %s vs %s", d1, d2)
	}
	if n1 != n2 || n1 != int64(len(body)) {
		t.Errorf("sizes wrong: %d, %d, want %d", n1, n2, len(body))
	}
	// The point of content addressing: the same bytes cost disk once.
	if s.Count() != 1 {
		t.Errorf("stored %d blobs for identical content, want 1", s.Count())
	}
	if s.Bytes() != int64(len(body)) {
		t.Errorf("accounted %d bytes, want %d", s.Bytes(), len(body))
	}
}

func TestDigestIsOfTheContentNotTheClaim(t *testing.T) {
	s, _ := Open(t.TempDir())

	// SHA-256 of "hello", so a caller cannot store one thing under another's name.
	d, _, err := s.Put(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if string(d) != want {
		t.Errorf("digest = %s, want %s", d, want)
	}
}

func TestNoTemporaryFilesSurviveAPut(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)

	if _, _, err := s.Put(strings.NewReader("content")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A leftover temp file would be served as though complete after a restart.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Errorf("temporary files left behind: %v", matches)
	}
}

func TestReopenRediscoversContent(t *testing.T) {
	dir := t.TempDir()
	first, _ := Open(dir)
	d, _, err := first.Put(strings.NewReader("durable"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The filesystem is the source of truth, so a fresh process must find the
	// same content without consulting any index.
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !second.Has(d) {
		t.Error("content not rediscovered after reopen")
	}
	if second.Count() != 1 || second.Bytes() != int64(len("durable")) {
		t.Errorf("reindex wrong: %d blobs, %d bytes", second.Count(), second.Bytes())
	}
}

func TestMalformedDigestIsRejected(t *testing.T) {
	s, _ := Open(t.TempDir())

	// A digest becomes part of a filesystem path, so traversal must not be
	// possible through it.
	for _, bad := range []Digest{"", "zz", "../../etc/passwd", Digest(strings.Repeat("g", 64))} {
		if bad.Valid() {
			t.Errorf("%q should not validate", bad)
		}
		if s.Has(bad) {
			t.Errorf("Has(%q) should be false", bad)
		}
		if _, err := s.Open(bad); err == nil {
			t.Errorf("Open(%q) should fail", bad)
		}
	}
}

func TestDeleteFreesAccounting(t *testing.T) {
	s, _ := Open(t.TempDir())
	d, n, _ := s.Put(strings.NewReader("transient"))

	if err := s.Delete(d); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Has(d) || s.Count() != 0 || s.Bytes() != 0 {
		t.Errorf("after delete: has=%v count=%d bytes=%d (stored %d)",
			s.Has(d), s.Count(), s.Bytes(), n)
	}
	// Deleting what is absent is not an error; eviction races would otherwise
	// need to care.
	if err := s.Delete(d); err != nil {
		t.Errorf("second Delete should be a no-op, got %v", err)
	}
}
