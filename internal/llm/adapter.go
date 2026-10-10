package llm

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"
)

type Response struct {
	Choices []*Message
	Usage   TokensUsage
}

type TokensUsage struct {
	Input  int
	Output int
}

func (r *Response) AddMessage(message *Message) {
	r.Choices = append(r.Choices, message)
}

// SpeechResult is one synthesized clip.
type SpeechResult struct {
	Audio      []byte      // OGG/Opus bytes (chat voice-message format)
	MimeType   string      // e.g. "audio/ogg"
	DurationMs int         // 0 when unknown (e.g. the OpenAI path)
	Model      string      // the TTS model actually used (not the chat model)
	Usage      TokensUsage // zero when the vendor reports no token accounting
}

// ClientAdapter is the proxy's slice of the shared adapter interface: text
// generation only. Files and transcription stay in the client services, whose
// download URLs carry credentials the proxy must not see.
type ClientAdapter interface {
	GenerateText(chat *ChatContext) (*Response, error)
}

// StreamChunk is one incremental delta from a streaming model call.
type StreamChunk struct {
	Delta          string
	FinishReason   string
	Usage          *TokensUsage
	ToolCallChunks []*ToolCallChunk
}

// StreamAdapter is an optional adapter capability for incremental text generation streaming.
type StreamAdapter interface {
	GenerateTextStream(ctx context.Context, chat *ChatContext, onChunk func(StreamChunk) error) error
}

// SpeechSynthesizer is an optional adapter capability, discovered by type
// assertion so the ClientAdapter contract stays untouched.
type SpeechSynthesizer interface {
	SynthesizeSpeech(text, language, userID string, meta map[string]string) (*SpeechResult, error)
}

// BatchSubmitItem is one request in a batch submission.
type BatchSubmitItem struct {
	CustomID string
	Chat     *ChatContext
}

// BatchJobStatus is the vendor status for a batch job.
type BatchJobStatus struct {
	State       string // PENDING, RUNNING, SUCCEEDED, FAILED, CANCELLED, EXPIRED
	TotalCount  int32
	DoneCount   int32
	FailedCount int32
	CompletedAt *time.Time
	Error       string
}

// BatchItemError represents an error for one item in a batch.
type BatchItemError struct {
	Code    int32  `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// NewBatchItemError builds a BatchItemError from a standard gRPC status code.
func NewBatchItemError(code codes.Code, reason, message string) *BatchItemError {
	return &BatchItemError{
		Code:    int32(code),
		Reason:  reason,
		Message: message,
	}
}

// JSON serializes the error for storage.
func (e *BatchItemError) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}

// BatchItemResult is the outcome of one item in a completed batch.
type BatchItemResult struct {
	CustomID string
	Response *Response
	Error    *BatchItemError
}

// BatchAdapter is the generic interface for batch generation vendors.
type BatchAdapter interface {
	SubmitBatch(ctx context.Context, batchID string, items []*BatchSubmitItem) (vendorJobID string, err error)
	GetBatch(ctx context.Context, vendorJobID string) (*BatchJobStatus, error)
	CancelBatch(ctx context.Context, vendorJobID string) error
	FetchBatchResults(ctx context.Context, vendorJobID string, items []*BatchSubmitItem) ([]*BatchItemResult, error)
}
