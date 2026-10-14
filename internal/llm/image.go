package llm

import (
	"context"
	"errors"
)

// ErrImageGenerationUnsupported marks an adapter with no native image model.
var ErrImageGenerationUnsupported = errors.New("image generation not supported by this adapter")

// ImageResult is one generated image.
type ImageResult struct {
	Data     []byte
	MimeType string      // e.g. "image/png"
	Model    string      // the image model actually used (not the chat model)
	Usage    TokensUsage // zero when the vendor reports no token accounting
}

// ImageGenerator is an optional adapter capability, discovered by type
// assertion so the ClientAdapter contract stays untouched. meta carries usage
// attribution (e.g. genjob ids), like SynthesizeSpeech.
type ImageGenerator interface {
	GenerateImage(ctx context.Context, prompt, userID string, meta map[string]string) (*ImageResult, error)
}
