package llm

import (
	"context"
	"errors"
)

// ErrJudgmentUnsupported marks an adapter with no judgment model.
var ErrJudgmentUnsupported = errors.New("judgment not supported by this adapter")

// Judgment question types. A judge vendor answers typed questions about a
// state and returns calibrated probabilities; it does not write text.
const (
	JudgeNoul   = "noul"   // does this hold? -> probability of yes
	JudgeChoice = "choice" // which one? -> option id + distribution
	JudgeScore  = "score"  // how much? -> weighted position on ordered levels
)

// JudgeQuestion is one typed question. Which fields carry meaning depends on
// Type: Options for choice, Levels for score, the criteria pair for noul.
type JudgeQuestion struct {
	Type          string
	Instructions  string
	Options       map[string]string
	Levels        []string
	TrueCriteria  string
	FalseCriteria string
}

// Judge state media kinds.
const (
	JudgeMediaText      = "text"
	JudgeMediaImage     = "image"
	JudgeMediaAudio     = "audio"
	JudgeMediaEmbedding = "embedding"
)

// JudgeMediaPart is one piece of multimodal judge state. Which fields carry
// meaning depends on Type, as with JudgeQuestion.
type JudgeMediaPart struct {
	Type     string
	Text     string
	MimeType string
	Data     []byte
	URL      string // https only; forwarded to the vendor, never fetched
	Detail   string // image: "auto", "low", "high"
	Format   string // audio: "wav", "mp3"
	// Embedding is a latent vector; EmbeddingModel names the encoder so a
	// vendor can reject a space it was not trained on.
	Embedding      []float32
	EmbeddingModel string
}

// JudgeRequest is one call's worth of judgment. Every question is answered
// against the same state, in parallel and for one state's worth of tokens.
type JudgeRequest struct {
	// StateJSON is the caller's state, already encoded. Vendors take it
	// verbatim, so the proxy never reshapes it.
	StateJSON string
	// StateMedia is multimodal state judged alongside StateJSON. Adapters that
	// cannot forward a part must refuse the call rather than judge without it.
	StateMedia []*JudgeMediaPart
	Questions  map[string]*JudgeQuestion
	// ActionID attributes the call in usage, as ChatContext does for text.
	ActionID string
}

// JudgeAnswer is one question's typed answer. Noul carries a probability;
// choice and score carry a distribution plus how concentrated it is.
type JudgeAnswer struct {
	Type          string
	Choice        string
	Noul          float64
	Score         float64
	Probabilities map[string]float64
	Confidence    float64
	Legend        map[string]string
}

// JudgeResult is one judgment call's outcome.
type JudgeResult struct {
	Answers map[string]*JudgeAnswer
	Model   string      // the judge model actually used
	Usage   TokensUsage // judge vendors bill input only, so Output is usually 0
}

// Judge is an optional adapter capability, discovered by type assertion so the
// ClientAdapter contract stays untouched. meta carries usage attribution, like
// SynthesizeSpeech and GenerateImage.
//
// Unlike the other modalities this one takes a context: a judgment is a
// decision on someone's request path, and when that caller gives up the vendor
// call should stop rather than run on and be billed for an answer nobody reads.
type Judge interface {
	Judge(ctx context.Context, req *JudgeRequest, userID string, meta map[string]string) (*JudgeResult, error)
}
