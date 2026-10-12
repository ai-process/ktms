package adapters

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// executeJudgeRequest issues a POST with a Bearer token, enforcing a 1MB response limit
// and capping error snippets at 300 characters to prevent memory and log bloat.
func executeJudgeRequest(ctx context.Context, client *http.Client, url, apiKey string, body []byte) ([]byte, int, error) {
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, judgeTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

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
