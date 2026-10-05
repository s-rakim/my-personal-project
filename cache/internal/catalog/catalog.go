// Package catalog maps the names clients ask for onto stored content, and
// decides what to discard when the disk fills.
//
// It is separate from the store because the two have different cardinalities.
// Many names can point at one blob, which is the whole point of content
// addressing: the same package pulled by thirty machines from three mirror URLs
// is three catalog entries and one stored copy. Deleting a name must therefore
// not delete the bytes unless no other name still wants them.
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/cache/internal/store"
)

// Entry is one named piece of content.
type Entry struct {
	Name   string       `json:"name"`
	Digest store.Digest `json:"digest"`
	Size   int64        `json:"size"`

	ContentType string `json:"content_type,omitempty"`
	UpstreamURL string `json:"upstream_url,omitempty"`

	// Priority keeps valuable content resident. Curriculum material should
	// outrank whatever someone browsed once.
	Priority int `json:"priority"`

	// Pinned content is never evicted, however old or cold. This is what a
	// teacher sets on the material a lesson depends on, so an unrelated download
	// cannot quietly push it off the disk the night before a class.
	Pinned bool `json:"pinned"`

	AddedAt    time.Time `json:"added_at"`
	LastAccess time.Time `json:"last_access"`
	Hits       int64     `json:"hits"`
}

// Stats is what the cache has done.
type Stats struct {
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`

	// BytesServed is everything handed to clients. BytesFetched is what the
	// uplink actually carried.
	BytesServed  int64 `json:"bytes_served"`
	BytesFetched int64 `json:"bytes_fetched"`

	Entries     int   `json:"entries"`
	Blobs       int   `json:"blobs"`
	StoredBytes int64 `json:"stored_bytes"`
	Evictions   int64 `json:"evictions"`
}

// BytesSaved is the uplink traffic the cache avoided.
//
// This is the number that justifies the machine. A school on a 10 Mbps line that
// serves 400 GB and fetches 40 has effectively been running at 100 Mbps for the
// cost of a disk.
func (s Stats) BytesSaved() int64 {
	saved := s.BytesServed - s.BytesFetched
	if saved < 0 {
		return 0
	}
	return saved
}

// HitRate is the fraction of requests served without touching the uplink.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// Multiplier is how much more content was delivered than was fetched.
func (s Stats) Multiplier() float64 {
	if s.BytesFetched == 0 {
		if s.BytesServed > 0 {
			return float64(s.BytesServed)
		}
		return 1
	}
	return float64(s.BytesServed) / float64(s.BytesFetched)
}

// Catalog is the name index over a store.
type Catalog struct {
	mu      sync.RWMutex
	path    string
	entries map[string]*Entry

	// refs counts how many names point at each digest, so content is only
	// deleted when the last name referring to it goes.
	refs  map[store.Digest]int
	stats Stats
}

// Open loads a catalog, creating an empty one if absent.
func Open(path string) (*Catalog, error) {
	c := &Catalog{
		path:    path,
		entries: make(map[string]*Entry),
		refs:    make(map[store.Digest]int),
	}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return c, nil
	}

	var doc struct {
		Entries []*Entry `json:"entries"`
		Stats   Stats    `json:"stats"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("catalog: parse %s: %w", path, err)
	}
	for _, e := range doc.Entries {
		c.entries[e.Name] = e
		c.refs[e.Digest]++
	}
	c.stats = doc.Stats
	return c, nil
}

// Reconcile drops entries whose content is no longer in the store.
//
// Worth running at startup, because the store scans the filesystem and the
// catalog does not: if a blob was deleted out from under us, the entry pointing
// at it would otherwise report a hit and then fail to serve.
func (c *Catalog) Reconcile(s *store.Store) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	var dropped int
	for name, e := range c.entries {
		if !s.Has(e.Digest) {
			delete(c.entries, name)
			c.release(e.Digest)
			dropped++
		}
	}
	return dropped
}

// Lookup returns an entry and records a hit.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[name]
	if !ok {
		c.stats.Misses++
		return Entry{}, false
	}
	e.Hits++
	e.LastAccess = time.Now().UTC()
	c.stats.Hits++
	c.stats.BytesServed += e.Size
	return *e, true
}

// Peek returns an entry without affecting statistics.
func (c *Catalog) Peek(name string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[name]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Put records a name pointing at stored content.
//
// fetchedBytes is what the uplink carried to obtain it, and is zero when the
// content was already present under another name. Keeping the two apart is what
// makes the saved-bytes figure honest rather than flattering.
func (c *Catalog) Put(e Entry, fetchedBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	if prior, ok := c.entries[e.Name]; ok {
		// Replacing a name: drop its old reference first, or the old blob is
		// pinned in place by a reference nothing points at any more.
		c.release(prior.Digest)
	}
	if e.AddedAt.IsZero() {
		e.AddedAt = now
	}
	e.LastAccess = now

	stored := e
	c.entries[e.Name] = &stored
	c.refs[e.Digest]++
	c.stats.BytesFetched += fetchedBytes
}

// release decrements a digest's reference count. Caller holds the lock.
func (c *Catalog) release(d store.Digest) {
	if n, ok := c.refs[d]; ok {
		if n <= 1 {
			delete(c.refs, d)
		} else {
			c.refs[d] = n - 1
		}
	}
}

// Referenced reports whether any name still points at some content.
func (c *Catalog) Referenced(d store.Digest) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.refs[d] > 0
}

// Pin marks an entry as never-evict.
func (c *Catalog) Pin(name string, pinned bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok {
		return false
	}
	e.Pinned = pinned
	return true
}

// Entries returns every entry, most recently used first.
func (c *Catalog) Entries() []Entry {
	c.mu.RLock()
	out := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, *e)
	}
	c.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].LastAccess.After(out[j].LastAccess) })
	return out
}

// Stats returns a snapshot, with live store figures folded in.
func (c *Catalog) Stats(s *store.Store) Stats {
	c.mu.RLock()
	out := c.stats
	out.Entries = len(c.entries)
	c.mu.RUnlock()

	out.Blobs = s.Count()
	out.StoredBytes = s.Bytes()
	return out
}

// Evict frees space until the store is under budgetBytes, and returns what it
// removed.
//
// Order is deliberate: pinned content is untouchable, then the lowest priority
// goes first, and only within a priority does least-recently-used decide. A
// plain LRU would evict last term's curriculum because nobody opened it over the
// holidays, which is exactly the content most worth keeping on a bad line.
func (c *Catalog) Evict(s *store.Store, budgetBytes int64) ([]Entry, error) {
	if budgetBytes <= 0 || s.Bytes() <= budgetBytes {
		return nil, nil
	}

	c.mu.Lock()
	candidates := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		if !e.Pinned {
			candidates = append(candidates, *e)
		}
	}
	c.mu.Unlock()

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		if !candidates[i].LastAccess.Equal(candidates[j].LastAccess) {
			return candidates[i].LastAccess.Before(candidates[j].LastAccess)
		}
		return candidates[i].Name < candidates[j].Name
	})

	var removed []Entry
	for _, e := range candidates {
		if s.Bytes() <= budgetBytes {
			break
		}

		c.mu.Lock()
		current, stillThere := c.entries[e.Name]
		if !stillThere {
			c.mu.Unlock()
			continue
		}
		digest := current.Digest
		delete(c.entries, e.Name)
		c.release(digest)
		orphaned := c.refs[digest] == 0
		c.stats.Evictions++
		c.mu.Unlock()

		// Only delete the bytes once no name wants them. Otherwise evicting one
		// of thirty names for the same package would take the package away from
		// the other twenty-nine.
		if orphaned {
			if err := s.Delete(digest); err != nil {
				return removed, err
			}
		}
		removed = append(removed, e)
	}
	return removed, nil
}

// Save persists the catalog atomically.
func (c *Catalog) Save() error {
	if c.path == "" {
		return nil
	}

	c.mu.RLock()
	doc := struct {
		Entries []*Entry `json:"entries"`
		Stats   Stats    `json:"stats"`
	}{Stats: c.stats}
	for _, e := range c.entries {
		doc.Entries = append(doc.Entries, e)
	}
	c.mu.RUnlock()

	sort.Slice(doc.Entries, func(i, j int) bool { return doc.Entries[i].Name < doc.Entries[j].Name })

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("catalog: encode: %w", err)
	}
	if dir := filepath.Dir(c.path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("catalog: create %s: %w", dir, err)
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o640); err != nil {
		return fmt.Errorf("catalog: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, c.path)
}
