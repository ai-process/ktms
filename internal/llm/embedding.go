package llm

import (
	"context"
	"errors"
)

// ErrEmbeddingUnsupported marks an adapter that cannot produce text embeddings.
var ErrEmbeddingUnsupported = errors.New("embeddings not supported by this adapter")

// EmbeddingItem is one vector embedding corresponding to an input item.
type EmbeddingItem struct {
	Index  int
	Values []float32
}

// EmbeddingRequest is one call's worth of text embeddings.
type EmbeddingRequest struct {
	Inputs     []string
	Model      string
	Dimensions int
	InputType  string // "query" | "document"
	ActionID   string
}

// EmbeddingResult holds the computed vector embeddings and token usage.
type EmbeddingResult struct {
	Data  []EmbeddingItem
	Model string
	Usage TokensUsage
}

// EmbeddingAdapter is an optional adapter capability, discovered by type assertion.
type EmbeddingAdapter interface {
	CreateEmbeddings(ctx context.Context, req *EmbeddingRequest, userID string, meta map[string]string) (*EmbeddingResult, error)
}
