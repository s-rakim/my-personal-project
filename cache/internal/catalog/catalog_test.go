package catalog

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s-rakim/my-personal-project/cache/internal/store"
)

func newPair(t *testing.T) (*store.Store, *Catalog) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	c, err := Open(filepath.Join(dir, "catalog.json"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	return s, c
}

func put(t *testing.T, s *store.Store, c *Catalog, name, body string, priority int, pinned bool) {
	t.Helper()
	d, n, err := s.Put(strings.NewReader(body))
	if err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	c.Put(Entry{Name: name, Digest: d, Size: n, Priority: priority, Pinned: pinned}, n)
}

// The property that makes deduplication safe: evicting one name that shares
// content with another must not take the content away from the other.
func TestEvictingOneOfTwoNamesKeepsTheSharedContent(t *testing.T) {
	s, c := newPair(t)
	body := strings.Repeat("shared", 2000)

	put(t, s, c, "from-mirror-a", body, 0, false)
	// Make the second entry newer so eviction takes the first.
	time.Sleep(2 * time.Millisecond)
	put(t, s, c, "from-mirror-b", body, 0, false)

	if s.Count() != 1 {
		t.Fatalf("two names for identical content stored %d blobs, want 1", s.Count())
	}

	evicted, err := c.Evict(s, 1) // budget far below the stored size
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(evicted) == 0 {
		t.Fatal("nothing was evicted")
	}

	// The first eviction drops a name but the blob is still referenced.
	if len(evicted) == 1 {
		if s.Count() != 1 {
			t.Errorf("content deleted while another name still referenced it")
		}
		if _, ok := c.Peek(evicted[0].Name); ok {
			t.Errorf("%s should be gone from the catalog", evicted[0].Name)
		}
	}
	// Once every name is gone the bytes go too.
	if len(evicted) == 2 && s.Count() != 0 {
		t.Errorf("content survived after all names were evicted: %d blobs", s.Count())
	}
}

func TestPinnedContentIsNeverEvicted(t *testing.T) {
	s, c := newPair(t)
	put(t, s, c, "keep-me", strings.Repeat("a", 5000), 0, true)
	put(t, s, c, "drop-me", strings.Repeat("b", 5000), 9, false)

	evicted, err := c.Evict(s, 1)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for _, e := range evicted {
		if e.Name == "keep-me" {
			t.Fatal("pinned content was evicted")
		}
	}
	if _, ok := c.Peek("keep-me"); !ok {
		t.Error("pinned entry disappeared from the catalog")
	}
	// Even though pinning means the budget cannot be met.
	if len(evicted) != 1 || evicted[0].Name != "drop-me" {
		t.Errorf("expected only the unpinned entry to go, got %+v", evicted)
	}
}

// A plain LRU would discard the thing you deliberately kept because nobody
// opened it recently. Priority has to win first.
func TestLowPriorityGoesBeforeColdHighPriority(t *testing.T) {
	s, c := newPair(t)

	put(t, s, c, "important", strings.Repeat("a", 4000), 10, false)
	time.Sleep(2 * time.Millisecond)
	put(t, s, c, "trivial", strings.Repeat("b", 4000), 1, false)

	// "trivial" is newer, so LRU alone would keep it.
	evicted, err := c.Evict(s, 5000)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if len(evicted) != 1 {
		t.Fatalf("expected one eviction, got %d", len(evicted))
	}
	if evicted[0].Name != "trivial" {
		t.Errorf("evicted %q; low priority should go first regardless of recency",
			evicted[0].Name)
	}
}

func TestReconcileDropsEntriesWhoseContentIsGone(t *testing.T) {
	s, c := newPair(t)
	put(t, s, c, "orphan", "content", 0, false)

	entry, _ := c.Peek("orphan")
	if err := s.Delete(entry.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Without this the catalog reports a hit and then fails to serve it.
	if dropped := c.Reconcile(s); dropped != 1 {
		t.Errorf("Reconcile dropped %d entries, want 1", dropped)
	}
	if _, ok := c.Peek("orphan"); ok {
		t.Error("entry survived reconciliation despite missing content")
	}
}

func TestStatsReportSavingsHonestly(t *testing.T) {
	s, c := newPair(t)
	body := strings.Repeat("x", 1000)
	put(t, s, c, "thing", body, 0, false) // 1000 bytes fetched

	for i := 0; i < 4; i++ {
		if _, ok := c.Lookup("thing"); !ok {
			t.Fatal("expected a hit")
		}
	}
	c.Lookup("absent") // one miss

	st := c.Stats(s)
	if st.Hits != 4 || st.Misses != 1 {
		t.Errorf("hits=%d misses=%d, want 4 and 1", st.Hits, st.Misses)
	}
	if st.BytesFetched != 1000 {
		t.Errorf("fetched %d, want 1000", st.BytesFetched)
	}
	if st.BytesServed != 4000 {
		t.Errorf("served %d, want 4000", st.BytesServed)
	}
	// Four deliveries off one fetch is a 3000-byte saving and a 4x multiplier.
	if st.BytesSaved() != 3000 {
		t.Errorf("saved %d, want 3000", st.BytesSaved())
	}
	if got := st.Multiplier(); got < 3.99 || got > 4.01 {
		t.Errorf("multiplier %.2f, want 4", got)
	}
	if got := st.HitRate(); got < 0.79 || got > 0.81 {
		t.Errorf("hit rate %.2f, want 0.8", got)
	}
}

func TestCatalogSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	s, err := store.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	first, _ := Open(path)
	d, n, _ := s.Put(strings.NewReader("durable"))
	first.Put(Entry{Name: "thing", Digest: d, Size: n, Priority: 5, Pinned: true}, n)
	first.Lookup("thing")
	if err := first.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, ok := second.Peek("thing")
	if !ok {
		t.Fatal("entry did not survive restart")
	}
	if !e.Pinned || e.Priority != 5 {
		t.Errorf("metadata lost: %+v", e)
	}
	// Reference counts must be rebuilt, or eviction would delete content that
	// is still named.
	if !second.Referenced(d) {
		t.Error("reference count not rebuilt on load")
	}
	if st := second.Stats(s); st.Hits != 1 {
		t.Errorf("statistics not persisted: %+v", st)
	}
}
