package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestChunkTexts(t *testing.T) {
	texts := make([]string, 70)
	for i := range texts {
		texts[i] = "x"
	}
	texts[40] = strings.Repeat("y", promptChunkRunes+10) // too big alone: its own chunk
	got := chunkTexts(texts)
	want := []textChunk{{0, 32}, {32, 40}, {40, 41}, {41, 70}}
	if len(got) != len(want) {
		t.Fatalf("chunks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunks = %v, want %v", got, want)
		}
	}
}

type replyAdapter struct{ replies []string }

func (r *replyAdapter) GenerateText(_ context.Context, _ *ChatContext) (*Response, error) {
	text := r.replies[0]
	if len(r.replies) > 1 {
		r.replies = r.replies[1:]
	}
	resp := &Response{Usage: TokensUsage{Input: 10, Output: 5}}
	resp.AddMessage(&Message{Text: text, Type: MessageTypeBot})
	return resp, nil
}

func TestTranslateByPromptChecksReply(t *testing.T) {
	req := &TranslateRequest{Texts: []string{"one", "two"}, TargetLanguage: "de", SourceLanguage: "en"}
	cases := map[string]string{
		"not json":    `sorry, I cannot`,
		"count":       `{"translations":[{"id":0,"source_language":"en","text":"eins"}]}`,
		"missing id":  `{"translations":[{"source_language":"en","text":"eins"},{"id":1,"source_language":"en","text":"zwei"}]}`,
		"repeated id": `{"translations":[{"id":0,"source_language":"en","text":"eins"},{"id":0,"source_language":"en","text":"zwei"}]}`,
		"emptied":     `{"translations":[{"id":0,"source_language":"en","text":"eins"},{"id":1,"source_language":"en","text":" "}]}`,
	}
	for name, reply := range cases {
		res, err := TranslateByPrompt(context.Background(), &replyAdapter{replies: []string{reply}}, req, "u", nil)
		var mm *TranslationMismatchError
		if !errors.As(err, &mm) || !ShouldSkipToNextModel(err) {
			t.Errorf("%s: err = %v, want a mismatch that skips to the next model", name, err)
		}
		// Both attempts were spent, so both are billed.
		if res == nil || res.Usage.Input != 20 {
			t.Errorf("%s: usage = %+v, want two attempts", name, res)
		}
	}
	res, err := TranslateByPrompt(context.Background(), &replyAdapter{replies: []string{
		"```json\n" + `{"translations":[{"id":1,"source_language":"en","text":"zwei"},{"id":0,"source_language":"xx","text":"eins"}]}` + "\n```",
	}}, req, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Translations[0].Text != "eins" || res.Translations[1].Text != "zwei" || res.Translations[0].DetectedSourceLanguage != "en" {
		t.Errorf("translations = %+v", res.Translations)
	}
	if res.Usage.Characters != 6 {
		t.Errorf("characters = %d, want 6", res.Usage.Characters)
	}
}
