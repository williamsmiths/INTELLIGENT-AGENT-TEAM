package embed

import (
	"container/list"
	"context"
	"crypto/sha256"
	"sync"
)

const defaultCacheSize = 1024 // about 4 MB of 1024-dimension vectors

// lru is a bounded least-recently-used cache of query embeddings (spec S2 B.1:
// "cache theo (model, hash văn bản) trong bộ nhớ, có giới hạn").
type lru struct {
	mu    sync.Mutex
	max   int
	order *list.List // front = most recent
	items map[cacheKey]*list.Element
}

type cacheKey struct {
	model string
	hash  [sha256.Size]byte
}

type cacheEntry struct {
	key cacheKey
	vec []float32
}

func newLRU(max int) *lru {
	if max <= 0 {
		max = defaultCacheSize
	}
	return &lru{max: max, order: list.New(), items: map[cacheKey]*list.Element{}}
}

func (l *lru) get(k cacheKey) ([]float32, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.items[k]
	if !ok {
		return nil, false
	}
	l.order.MoveToFront(el)
	return el.Value.(*cacheEntry).vec, true
}

func (l *lru) put(k cacheKey, vec []float32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.items[k]; ok {
		el.Value.(*cacheEntry).vec = vec
		l.order.MoveToFront(el)
		return
	}
	l.items[k] = l.order.PushFront(&cacheEntry{key: k, vec: vec})
	for l.order.Len() > l.max {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.items, oldest.Value.(*cacheEntry).key)
	}
}

func (l *lru) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

func (c *Client) queryCache() *lru {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = newLRU(c.CacheSize)
	}
	return c.cache
}

// EmbedQuery embeds one query text through the cache. The returned slice is
// shared with the cache: callers must not modify it. Errors are not cached.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if c == nil {
		return nil, ErrUnavailable
	}
	text = Clip(text)
	key := cacheKey{model: c.Model, hash: sha256.Sum256([]byte(text))}
	cache := c.queryCache()
	if vec, ok := cache.get(key); ok {
		return vec, nil
	}
	vecs, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	cache.put(key, vecs[0])
	return vecs[0], nil
}

// CachedQueries is the number of query embeddings currently cached.
func (c *Client) CachedQueries() int { return c.queryCache().len() }
