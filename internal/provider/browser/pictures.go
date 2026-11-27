package browser

import "sync"

// pictureCacheMax bounds the pictures an Adapter keeps: a Dot's avatar is a few kilobytes, and an account has at most
// the 25 Dots its list reads.
const pictureCacheMax = 64

// pictureCache keeps pictures made in the page (data URLs) by a key that names the picture's content (a Dot's
// avatar file), so the next sync can name the keys it already has and the page skips fetching and scaling them. It
// lives as long as the program: after a restart the first sync makes each picture once again.
type pictureCache struct {
	mu    sync.Mutex
	max   int
	order []string
	byKey map[string]string
}

func newPictureCache(max int) *pictureCache {
	return &pictureCache{max: max, byKey: map[string]string{}}
}

func (c *pictureCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.byKey[key]
	return v, ok
}

// keys are the keys kept, oldest first.
func (c *pictureCache) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(make([]string, 0, len(c.order)), c.order...)
}

// put keeps v under key, dropping the oldest picture once more than max are kept.
func (c *pictureCache) put(key, v string) {
	if key == "" || v == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byKey[key]; !ok {
		c.order = append(c.order, key)
	}
	c.byKey[key] = v
	for len(c.order) > c.max {
		delete(c.byKey, c.order[0])
		c.order = c.order[1:]
	}
}
