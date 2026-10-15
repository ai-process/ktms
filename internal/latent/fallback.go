package latent

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

const fallbackWarnEvery = 10 * time.Second

// Fallback serves from primary and switches to secondary when it fails. The
// embeddings carry their model, so vectors from the two never get compared.
type Fallback struct {
	primary   Encoder
	secondary Encoder
	lastWarn  atomic.Int64
}

// NewFallback chains primary (typically HTTP/JEPA) to secondary (typically local).
func NewFallback(primary, secondary Encoder) *Fallback {
	return &Fallback{primary: primary, secondary: secondary}
}

func (f *Fallback) Encode(ctx context.Context, texts []string) ([]Embedding, error) {
	out, err := f.primary.Encode(ctx, texts)
	if err == nil {
		return out, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	// An encoder outage would otherwise log once per request.
	now := time.Now().UnixNano()
	if last := f.lastWarn.Load(); now-last >= int64(fallbackWarnEvery) && f.lastWarn.CompareAndSwap(last, now) {
		log.Warn().Err(err).Int("texts", len(texts)).Msg("latent encoder failed, using fallback encoder")
	}
	return f.secondary.Encode(ctx, texts)
}
