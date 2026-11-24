package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ai-process/ktms/internal/llm"
)

// The MT contract KTMS calls on a node engine (ktms-mt or any server that
// implements it): POST {base}/v1/translate with this body.
type mtRequest struct {
	Model          string   `json:"model"`
	Texts          []string `json:"texts"`
	SourceLanguage string   `json:"source_language,omitempty"`
	TargetLanguage string   `json:"target_language"`
	Format         string   `json:"format,omitempty"`
}

type mtResponse struct {
	Model        string `json:"model"`
	Translations []struct {
		Text                   string `json:"text"`
		DetectedSourceLanguage string `json:"detected_source_language"`
	} `json:"translations"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		Characters   int `json:"characters"`
	} `json:"usage"`
	Error *mtError `json:"error"`
}

type mtError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxMTResponse bounds a reply: 128 texts of 256 KiB in all, translated into
// a longer script, stay well under it.
const maxMTResponse = 8 << 20

// MTError is a failed MT call with the engine's status, so retry and
// fallback classify it like any vendor error.
type MTError struct {
	Status  int
	Code    string
	Message string
}

func (e *MTError) Error() string {
	return fmt.Sprintf("mt engine: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

func (e *MTError) HTTPStatusCode() int { return e.Status }

// TranslateHTTP calls an MT engine. An unknown pair and a missing route are
// the sentinel errors that move the chain on; 429 and 5xx stay retryable.
func TranslateHTTP(ctx context.Context, client *http.Client, baseURL, token, model string, req *llm.TranslateRequest) (*llm.TranslateResult, error) {
	if client == nil {
		client = llm.VendorHTTPClient()
	}
	body, err := json.Marshal(mtRequest{
		Model: model, Texts: req.Texts, SourceLanguage: req.SourceLanguage,
		TargetLanguage: req.TargetLanguage, Format: req.Format,
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(baseURL, "/")
	url = strings.TrimSuffix(url, "/v1") + "/v1/translate"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMTResponse))
	if err != nil {
		return nil, err
	}
	var out mtResponse
	decodeErr := json.Unmarshal(raw, &out)
	// The node gateway may have sent 200 to keep a slow call alive; a failure
	// then arrives in the body.
	if resp.StatusCode/100 != 2 || out.Error != nil {
		e := &MTError{Status: resp.StatusCode, Message: snippet(raw)}
		if out.Error != nil {
			e.Code, e.Message = out.Error.Code, out.Error.Message
			if resp.StatusCode/100 == 2 {
				e.Status = http.StatusBadGateway
			}
		}
		switch {
		case e.Code == "unsupported_language_pair":
			return nil, fmt.Errorf("%w: %v", llm.ErrUnsupportedLanguagePair, e)
		case e.Status == http.StatusNotFound:
			return nil, fmt.Errorf("%w: %v", llm.ErrTranslationUnsupported, e)
		}
		return nil, e
	}
	if decodeErr != nil {
		return nil, &MTError{Status: http.StatusBadGateway, Message: "unreadable reply: " + decodeErr.Error()}
	}
	if len(out.Translations) != len(req.Texts) {
		return nil, &llm.TranslationMismatchError{Reason: fmt.Sprintf("engine returned %d translations for %d texts", len(out.Translations), len(req.Texts))}
	}
	res := &llm.TranslateResult{
		Model:        out.Model,
		Translations: make([]llm.Translation, len(out.Translations)),
		Usage: llm.TokensUsage{
			Input:      out.Usage.InputTokens,
			Output:     out.Usage.OutputTokens,
			Characters: out.Usage.Characters,
		},
	}
	if res.Usage.Characters == 0 {
		res.Usage.Characters = llm.SourceCharacters(req.Texts)
	}
	for i, t := range out.Translations {
		detected := t.DetectedSourceLanguage
		if detected == "" {
			detected = req.SourceLanguage
		}
		res.Translations[i] = llm.Translation{Text: t.Text, DetectedSourceLanguage: detected}
	}
	return res, nil
}

func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
