package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Event is one journal entry of a conversation.
type Event struct {
	Type      string    `json:"type"` // task.created, task.<status>
	At        time.Time `json:"at"`
	TaskID    string    `json:"task_id"`
	ContextID string    `json:"context_id"`
	From      string    `json:"from"`
	FromOwner string    `json:"from_owner,omitempty"`
	To        string    `json:"to"`
	ToOwner   string    `json:"to_owner,omitempty"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Depth     int32     `json:"depth"`
	Bytes     int       `json:"bytes"`
	// Only at recording level full; cut to the event limit, with the hash of the whole.
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// Journal keeps conversation events for audit and for exporters.
type Journal interface {
	Record(ctx context.Context, ev Event) error
}

// RedisJournal is the best-effort journal: a capped Redis stream. The oldest
// events go first when it is full, and the write never blocks a conversation
// for longer than the client's timeout.
type RedisJournal struct {
	rdb      redis.Cmdable
	key      string
	maxLen   int64
	maxEvent int
	timeout  time.Duration
}

func NewRedisJournal(rdb redis.Cmdable, key string, maxLen int64, maxEventBytes int) *RedisJournal {
	if maxLen <= 0 {
		maxLen = 100_000
	}
	if maxEventBytes <= 0 {
		maxEventBytes = 16 << 10
	}
	return &RedisJournal{rdb: rdb, key: key, maxLen: maxLen, maxEvent: maxEventBytes, timeout: time.Second}
}

func (j *RedisJournal) Record(ctx context.Context, ev Event) error {
	if len(ev.Text) > j.maxEvent {
		sum := sha256.Sum256([]byte(ev.Text))
		ev.Text, ev.Truncated, ev.SHA256 = cut(ev.Text, j.maxEvent), true, hex.EncodeToString(sum[:])
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.timeout)
	defer cancel()
	return j.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: j.key, MaxLen: j.maxLen, Approx: true,
		Values: map[string]any{"event": b},
	}).Err()
}

// cut shortens s to at most n bytes without splitting a UTF-8 character.
func cut(s string, n int) string {
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
