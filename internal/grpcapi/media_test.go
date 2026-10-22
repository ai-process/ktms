package grpcapi

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/audio"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

type fakeTTSAdapter struct {
	err error
}

func (f *fakeTTSAdapter) GenerateText(context.Context, *llm.ChatContext) (*llm.Response, error) {
	return nil, nil
}

func (f *fakeTTSAdapter) SynthesizeSpeech(context.Context, string, string, string, map[string]string) (*llm.SpeechResult, error) {
	return nil, f.err
}

func TestSynthesizeSpeech_ErrFFmpegMissing(t *testing.T) {
	model := &proxydb.Model{
		ID:           "tts-model",
		Vendor:       registry.VendorGoogle,
		Efforts:      []string{registry.EffortLow},
		Capabilities: []string{registry.CapabilityTTS},
		Enabled:      true,
	}
	r := &proxydb.Rule{
		Name:       "catchall",
		Effort:     registry.EffortLow,
		Attributes: map[string]string{"modality": "tts"},
		Enabled:    true,
		Use:        []string{"tts-model"},
	}
	snap := registry.NewSnapshotForTest([]*proxydb.Model{model}, []*proxydb.Rule{r}, map[string]map[string]string{
		"key1": {registry.VendorGoogle: "secret"},
	}, nil)
	snap.SetAdapter("key1", "tts-model", &fakeTTSAdapter{err: audio.ErrFFmpegMissing})

	s := NewProxyServer(fixedSnapshot{snap}, nil)

	ctx := testIdentity()
	_, err := s.SynthesizeSpeech(ctx, &pb.SynthesizeSpeechRequest{
		Text: "Hello world",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st := status.Convert(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
	wantMsg := "ffmpeg_missing: ffmpeg is required for Gemini speech synthesis: install ffmpeg or use the 'coyl/ktms:audio' container image"
	if st.Message() != wantMsg {
		t.Errorf("message = %q, want %q", st.Message(), wantMsg)
	}
}
