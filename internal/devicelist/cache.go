package devicelist

import "sync"

// Cache bounds.
const (
	DefaultCacheLists = 8
	MaxCacheLists     = 64
)

// Lists is the lookup that Admit reads: the list held under an h(D), if any.
// *Cache implements it.
type Lists interface {
	Get(sha256 string) (*List, bool)
}

// hashMatchesPin is the hash-to-pin rule of O3.11 and step 5: the octets
// count as the list only when their SHA-256 is the pinned one. Cache.Add and
// Admit both call it. It is a variable only so that a control can plant its
// removal and prove that a named assertion fails; production never reassigns
// it.
var hashMatchesPin = func(octets []byte, pinned string) bool { return HashOf(octets) == pinned }

// Cache holds verified device lists keyed by h(D) (O3.12). It is bounded: at
// the bound, the least recently used list is dropped, and a later lookup of
// it misses, which Admit refuses as LIST_UNKNOWN. The cache has no fetch
// path: its only input is octets handed to Add, and it takes no URL, hint or
// location of any kind (O3.13, O3.14).
type Cache struct {
	mu    sync.Mutex
	max   int
	lists map[string]*List
	order []string // Least recently used first.
}

// NewCache returns a cache of at most max lists. A max of 0 selects
// DefaultCacheLists; a negative max or one above MaxCacheLists is refused as
// ErrInvalid, and no bound is replaced silently.
func NewCache(max int) (*Cache, error) {
	if max == 0 {
		max = DefaultCacheLists
	}
	if max < 1 || max > MaxCacheLists {
		return nil, ErrInvalid
	}
	return &Cache{max: max, lists: map[string]*List{}}, nil
}

// Add verifies octets against a pin that the caller read from the owner's
// name on its own node, and caches the list under h(D). The octets must hash
// to pin.SHA256 (O3.11) and the list's seq must be the pinned seq (O3.6),
// else DEVICE_LIST_MISMATCH; they must parse under section 3.2, else
// DEVICE_LIST_INVALID. The hash is compared first, so octets with one added
// space refuse as a mismatch. Nothing is cached on a refusal.
func (c *Cache) Add(octets []byte, pin Pin) (*List, error) {
	if !pin.valid() {
		return nil, DevicePinInvalid
	}
	if len(octets) > MaxListBytes {
		return nil, invalidList("size")
	}
	if !hashMatchesPin(octets, pin.SHA256) {
		return nil, DeviceListMismatch
	}
	list, err := Parse(octets)
	if err != nil {
		return nil, err
	}
	if list.seq != pin.Seq {
		return nil, DeviceListMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	hash := list.Hash()
	if _, held := c.lists[hash]; held {
		c.touch(hash)
		return c.lists[hash], nil
	}
	if len(c.order) >= c.max {
		delete(c.lists, c.order[0])
		c.order = c.order[1:]
	}
	c.lists[hash] = list
	c.order = append(c.order, hash)
	return list, nil
}

// Get returns the list cached under the hash, and marks it used.
func (c *Cache) Get(sha256 string) (*List, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.lists[sha256]
	if ok {
		c.touch(sha256)
	}
	return list, ok
}

// Len is the number of cached lists.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lists)
}

func (c *Cache) touch(hash string) {
	for i, h := range c.order {
		if h == hash {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), hash)
			return
		}
	}
}
