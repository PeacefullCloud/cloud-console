package instances

import (
	"sync"
	"time"
)

// imageLabelTTL is short enough that a re-imported or renamed image shows up
// soon, long enough that a list of instances costs one lookup per image.
const imageLabelTTL = 5 * time.Minute

// imageLabelCache remembers image descriptions by fingerprint.
type imageLabelCache struct {
	mu      sync.Mutex
	entries map[string]imageLabelEntry
}

type imageLabelEntry struct {
	label string
	at    time.Time
}

func (c *imageLabelCache) get(fingerprint string, lookup func(string) string) string {
	c.mu.Lock()
	if e, ok := c.entries[fingerprint]; ok && time.Since(e.at) < imageLabelTTL {
		c.mu.Unlock()
		return e.label
	}
	c.mu.Unlock()

	// Not under the lock: the lookup is an API call.
	label := lookup(fingerprint)

	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]imageLabelEntry{}
	}
	c.entries[fingerprint] = imageLabelEntry{label: label, at: time.Now()}
	c.mu.Unlock()
	return label
}
