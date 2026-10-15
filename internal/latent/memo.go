package latent

import (
	"container/list"
	"context"
	"crypto/sha256"
	"sync"
)

// Memo is an in-memory LRU in front of an encoder. One request is encoded by
// both the latent guardrail and the semantic cache; the memo makes the second
// lookup free. Returned vectors are shared and must be treated as read-only.
type Memo struct {
	inner Encoder
	size  int

	mu    sync.Mutex
	order *list.List
	items map[[sha256.Size]byte]*list.Element
}

type memoEntry struct {
	key [sha256.Size]byte
	emb Embedding
}

// NewMemo caches up to size embeddings; size <= 0 disables caching.
func NewMemo(inner Encoder, size int) *Memo {
	return &Memo{inner: inner, size: size, order: list.New(), items: make(map[[sha256.Size]byte]*list.Element)}
}

func (m *Memo) Encode(ctx context.Context, texts []string) ([]Embedding, error) {
	if m.size <= 0 {
		return m.inner.Encode(ctx, texts)
	}
	out := make([]Embedding, len(texts))
	keys := make([][sha256.Size]byte, len(texts))
	var missIdx []int
	var missTexts []string

	m.mu.Lock()
	for i, t := range texts {
		keys[i] = sha256.Sum256([]byte(t))
		if el, ok := m.items[keys[i]]; ok {
			m.order.MoveToFront(el)
			out[i] = el.Value.(*memoEntry).emb
			continue
		}
		missIdx = append(missIdx, i)
		missTexts = append(missTexts, t)
	}
	m.mu.Unlock()

	if len(missTexts) == 0 {
		return out, nil
	}
	encoded, err := m.inner.Encode(ctx, missTexts)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for j, i := range missIdx {
		out[i] = encoded[j]
		if el, ok := m.items[keys[i]]; ok {
			el.Value.(*memoEntry).emb = encoded[j]
			m.order.MoveToFront(el)
			continue
		}
		m.items[keys[i]] = m.order.PushFront(&memoEntry{key: keys[i], emb: encoded[j]})
		if m.order.Len() > m.size {
			oldest := m.order.Back()
			m.order.Remove(oldest)
			delete(m.items, oldest.Value.(*memoEntry).key)
		}
	}
	return out, nil
}

// Len reports how many embeddings are cached.
func (m *Memo) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.order.Len()
}
