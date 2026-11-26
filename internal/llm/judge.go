package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrJudgmentUnsupported marks an adapter with no judgment model.
var ErrJudgmentUnsupported = errors.New("judgment not supported by this adapter")

// Judgment question types. A judge vendor answers typed questions about a
// state and returns calibrated probabilities; it does not write text.
const (
	JudgeNoul   = "noul"   // does this hold? -> probability of yes
	JudgeChoice = "choice" // which one? -> option id + distribution
	JudgeScore  = "score"  // how much? -> weighted position on ordered levels
	JudgeBox    = "box"    // where is it? -> one box on an image of the state
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
	// InstructionsJSON and CriteriaJSON are the structured forms (an object or
	// array, possibly naming images); when set they replace Instructions and
	// the per-type criteria fields.
	InstructionsJSON json.RawMessage
	CriteriaJSON     json.RawMessage
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
	// Name is how state, instructions and criteria refer to the part; "" for
	// an unnamed one.
	Name string
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
	Box           *Box // box questions only
	// ConfidenceUnset: the vendor gave no confidence, so Confidence means nothing.
	ConfidenceUnset bool
}

// Box is a box on an image: edges as fractions 0..1 of its height (Top,
// Bottom) and width (Left, Right) from the top-left corner.
type Box struct {
	Top, Bottom, Left, Right float64
}

// ErrInvalidMedia marks judge state media a vendor would refuse as it stands
// (format, size, orientation): the caller's to fix, so not a vendor failure.
var ErrInvalidMedia = errors.New("invalid media")

// InvalidMediaError says what is wrong with which part, for the caller. Part
// indexes StateMedia; Field names the request field, "state_media" when empty.
type InvalidMediaError struct {
	Field string
	Part  int
	Msg   string
}

func (e *InvalidMediaError) Error() string {
	field := e.Field
	if field == "" {
		field = "state_media"
	}
	return fmt.Sprintf("%s[%d]: %s", field, e.Part, e.Msg)
}
func (e *InvalidMediaError) Is(target error) bool { return target == ErrInvalidMedia }

// JudgeMediaChecker is an optional Judge capability: it says whether the
// vendor would refuse media before any call, so a batch fails before it bills.
type JudgeMediaChecker interface {
	CheckJudgeMedia(media []*JudgeMediaPart) error
}

// JudgeResult is one judgment call's outcome.
type JudgeResult struct {
	Answers map[string]*JudgeAnswer
	Model   string      // the judge model actually used
	Usage   TokensUsage // judge vendors bill input only, so Output is usually 0
}

// RolloutCandidate is one candidate action or branch in speculative evaluation.
type RolloutCandidate struct {
	ID          string
	ActionJSON  string
	ActionMedia []*JudgeMediaPart
	ActionText  string
}

// CandidateEvaluation holds evaluation answers, rank, and composite score.
type CandidateEvaluation struct {
	ID             string
	Answers        map[string]*JudgeAnswer
	CompositeScore float64
	Rank           int
}

// EvaluateRolloutsRequest evaluates multiple candidate rollouts against state and criteria.
type EvaluateRolloutsRequest struct {
	StateJSON  string
	StateMedia []*JudgeMediaPart
	Candidates []*RolloutCandidate
	Criteria   map[string]*JudgeQuestion
	ActionID   string
}

// EvaluateRolloutsResult is the outcome of candidate rollouts evaluation.
type EvaluateRolloutsResult struct {
	Evaluations     []*CandidateEvaluation
	BestCandidateID string
	Model           string
	Usage           TokensUsage
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

// RolloutJudge is an optional capability for adapters supporting native batch rollout scoring.
type RolloutJudge interface {
	EvaluateRollouts(ctx context.Context, req *EvaluateRolloutsRequest, userID string, meta map[string]string) (*EvaluateRolloutsResult, error)
}

// Flat returns q with its JSON forms read as text into the plain fields
// (compact JSON for objects, the string itself for a JSON string), for judges
// that take text and for what reads levels and options. The JSON stays set.
func (q *JudgeQuestion) Flat() *JudgeQuestion {
	if len(q.InstructionsJSON) == 0 && len(q.CriteriaJSON) == 0 {
		return q
	}
	out := *q
	if len(q.InstructionsJSON) > 0 {
		out.Instructions = JSONText(q.InstructionsJSON)
	}
	if len(q.CriteriaJSON) == 0 {
		return &out
	}
	switch q.Type {
	case JudgeChoice:
		var m map[string]json.RawMessage
		if json.Unmarshal(q.CriteriaJSON, &m) == nil {
			out.Options = make(map[string]string, len(m))
			for k, v := range m {
				out.Options[k] = JSONText(v)
			}
		}
	case JudgeScore:
		var a []json.RawMessage
		if json.Unmarshal(q.CriteriaJSON, &a) == nil {
			out.Levels = make([]string, len(a))
			for i, v := range a {
				out.Levels[i] = JSONText(v)
			}
		}
	case JudgeNoul:
		var m map[string]json.RawMessage
		if json.Unmarshal(q.CriteriaJSON, &m) == nil {
			out.TrueCriteria, out.FalseCriteria = JSONText(m["true"]), JSONText(m["false"])
		}
	}
	return &out
}

// JSONText is a JSON value as text: a string as itself, anything else compact.
func JSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) == nil {
		return b.String()
	}
	return string(raw)
}

// MapJSONStrings applies fn to every string value in raw (not to object
// keys, which name options) and returns the re-encoded JSON.
func MapJSONStrings(raw json.RawMessage, fn func(string) (string, error)) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var walk func(any) (any, error)
	walk = func(x any) (any, error) {
		switch t := x.(type) {
		case string:
			return fn(t)
		case []any:
			for i := range t {
				var err error
				if t[i], err = walk(t[i]); err != nil {
					return nil, err
				}
			}
		case map[string]any:
			for k := range t {
				var err error
				if t[k], err = walk(t[k]); err != nil {
					return nil, err
				}
			}
		}
		return x, nil
	}
	v, err := walk(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
