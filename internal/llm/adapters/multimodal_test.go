package adapters

import (
	"encoding/base64"
	"testing"

	"github.com/ai-process/llm-proxy/internal/llm"
)

func TestOpenAIAdapter_MultimodalConversion(t *testing.T) {
	adapter := &OpenAIAdapter{model: "gpt-4o"}

	msg := &llm.Message{
		Type: llm.MessageTypeUser,
		Parts: []*llm.ContentPart{
			{
				Type: llm.ContentPartTypeText,
				Text: "What is this?",
			},
			{
				Type:     llm.ContentPartTypeImage,
				MimeType: "image/png",
				Data:     []byte("png-bytes"),
				Detail:   llm.ImageDetailHigh,
			},
			{
				Type:     llm.ContentPartTypeAudio,
				MimeType: "audio/wav",
				Data:     []byte("wav-bytes"),
				Format:   "wav",
			},
			{
				Type:     llm.ContentPartTypeDocument,
				MimeType: "application/pdf",
				Data:     []byte("pdf-bytes"),
				FileName: "doc.pdf",
			},
		},
	}

	res := adapter.convertMessageToOpenAi(msg)
	if res.OfUser == nil {
		t.Fatal("expected OfUser message param")
	}
	parts := res.OfUser.Content.OfArrayOfContentParts
	if len(parts) != 4 {
		t.Fatalf("expected 4 content parts, got %d", len(parts))
	}

	// Text
	if parts[0].OfText == nil || parts[0].OfText.Text != "What is this?" {
		t.Errorf("part 0 text mismatch: %+v", parts[0])
	}

	// Image
	if parts[1].OfImageURL == nil || parts[1].OfImageURL.ImageURL.Detail != "high" {
		t.Errorf("part 1 image mismatch: %+v", parts[1])
	}
	expectedDataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	if parts[1].OfImageURL.ImageURL.URL != expectedDataURI {
		t.Errorf("part 1 URL = %q, want %q", parts[1].OfImageURL.ImageURL.URL, expectedDataURI)
	}

	// Audio
	if parts[2].OfInputAudio == nil || parts[2].OfInputAudio.InputAudio.Format != "wav" {
		t.Errorf("part 2 audio mismatch: %+v", parts[2])
	}

	// Document / File
	if parts[3].OfFile == nil || parts[3].OfFile.File.Filename.Value != "doc.pdf" {
		t.Errorf("part 3 file mismatch: %+v", parts[3])
	}
}

func TestGoogleAdapter_MultimodalConversion(t *testing.T) {
	adapter := &GoogleAdapter{model: "gemini-2.5-flash"}

	msg := &llm.Message{
		Type: llm.MessageTypeUser,
		Parts: []*llm.ContentPart{
			{
				Type: llm.ContentPartTypeText,
				Text: "Analyze this image",
			},
			{
				Type:     llm.ContentPartTypeImage,
				MimeType: "image/jpeg",
				Data:     []byte("jpeg-bytes"),
			},
			{
				Type:     llm.ContentPartTypeAudio,
				MimeType: "audio/mp3",
				Data:     []byte("mp3-bytes"),
			},
			{
				Type:     llm.ContentPartTypeDocument,
				MimeType: "application/pdf",
				Data:     []byte("pdf-bytes"),
			},
		},
	}

	contents := adapter.contextToContents([]*llm.Message{msg})
	if len(contents) != 1 {
		t.Fatalf("expected 1 content turn, got %d", len(contents))
	}
	turn := contents[0]
	if turn.Role != "user" {
		t.Errorf("role = %q, want user", turn.Role)
	}
	if len(turn.Parts) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(turn.Parts))
	}

	if turn.Parts[0].Text != "Analyze this image" {
		t.Errorf("part 0 text = %q", turn.Parts[0].Text)
	}
	if turn.Parts[1].InlineData == nil || turn.Parts[1].InlineData.MIMEType != "image/jpeg" {
		t.Errorf("part 1 inline data mismatch: %+v", turn.Parts[1].InlineData)
	}
	if turn.Parts[2].InlineData == nil || turn.Parts[2].InlineData.MIMEType != "audio/mp3" {
		t.Errorf("part 2 inline data mismatch: %+v", turn.Parts[2].InlineData)
	}
	if turn.Parts[3].InlineData == nil || turn.Parts[3].InlineData.MIMEType != "application/pdf" {
		t.Errorf("part 3 inline data mismatch: %+v", turn.Parts[3].InlineData)
	}
}

func TestAnthropicAdapter_MultimodalConversion(t *testing.T) {
	adapter := &AnthropicAdapter{model: "claude-3-7-sonnet-20250219"}

	msg := &llm.Message{
		Type: llm.MessageTypeUser,
		Parts: []*llm.ContentPart{
			{
				Type: llm.ContentPartTypeText,
				Text: "Look at the chart and report",
			},
			{
				Type:     llm.ContentPartTypeImage,
				MimeType: "image/png",
				Data:     []byte("png-bytes"),
			},
			{
				Type:     llm.ContentPartTypeDocument,
				MimeType: "application/pdf",
				Data:     []byte("pdf-bytes"),
			},
		},
	}

	messages, _ := adapter.contextToMessages([]*llm.Message{msg}, "")
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
}
