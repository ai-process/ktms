package adapters

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ai-process/ktms/internal/llm"
)

// judgeCall is one judge HTTP request: a JSON body by default, multipart for
// System One with images.
type judgeCall struct {
	body        []byte
	contentType string
	headers     map[string]string
	timeout     time.Duration // 0 = judgeTimeout
}

// executeJudgeRequest issues a JSON POST with a Bearer token.
func executeJudgeRequest(ctx context.Context, client *http.Client, url, apiKey string, body []byte) ([]byte, int, error) {
	return executeJudgeCall(ctx, client, url, apiKey, judgeCall{body: body, contentType: "application/json"})
}

// executeJudgeCall enforces a 1MB response limit and caps error snippets at
// 300 characters to prevent memory and log bloat.
func executeJudgeCall(ctx context.Context, client *http.Client, url, apiKey string, call judgeCall) ([]byte, int, error) {
	if client == nil {
		client = llm.VendorHTTPClient()
	}
	timeout := call.timeout
	if timeout <= 0 {
		timeout = judgeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(call.body))
	if err != nil {
		return nil, 0, err
	}
	if apiKey != "" { // System One adapters set their own scheme in call.headers
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("Content-Type", call.contentType)
	for k, v := range call.headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, resp.StatusCode, fmt.Errorf("%s", snippet)
	}
	return raw, resp.StatusCode, nil
}
