package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/ai-process/llm-proxy/internal/llm"
)

func TestParseGenerateContentResponse_MaxTokensTruncation(t *testing.T) {
	adapter := &GoogleAdapter{}

	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonMaxTokens,
				Content: &genai.Content{
					Parts: []*genai.Part{
						{Text: "incomplete senten"},
					},
				},
			},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     15,
			CandidatesTokenCount: 100,
		},
	}

	_, err := adapter.parseGenerateContentResponse(resp, genai.FinishReasonMaxTokens, nil)
	if err == nil {
		t.Fatal("expected ErrOutputTruncated, got nil")
	}
	if !errors.Is(err, llm.ErrOutputTruncated) {
		t.Fatalf("expected llm.ErrOutputTruncated, got %v", err)
	}
}

func TestMapJobState(t *testing.T) {
	if s := mapJobState(genai.JobStateCancelling); s != "RUNNING" {
		t.Fatalf("expected JobStateCancelling to map to RUNNING (unfinished), got %s", s)
	}
	if s := mapJobState(genai.JobStateCancelled); s != "CANCELLED" {
		t.Fatalf("expected JobStateCancelled to map to CANCELLED, got %s", s)
	}
	if s := mapJobState(genai.JobStateRunning); s != "RUNNING" {
		t.Fatalf("expected JobStateRunning to map to RUNNING, got %s", s)
	}
}

func TestParseGenerateContentResponse_UsageAndContent(t *testing.T) {
	adapter := &GoogleAdapter{}

	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonStop,
				Content: &genai.Content{
					Parts: []*genai.Part{
						{Text: "hello world"},
					},
				},
			},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     20,
			CandidatesTokenCount: 5,
		},
	}

	parsed, err := adapter.parseGenerateContentResponse(resp, genai.FinishReasonStop, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parsed.Choices) != 1 || parsed.Choices[0].Text != "hello world" {
		t.Fatalf("unexpected choices: %+v", parsed.Choices)
	}
	if parsed.Usage.Input != 20 || parsed.Usage.Output != 5 {
		t.Fatalf("unexpected usage: %+v", parsed.Usage)
	}
}

func TestParseGenerateContentResponse_SoftSchemaFailure(t *testing.T) {
	adapter := &GoogleAdapter{softSchema: true}

	resp := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonStop,
				Content: &genai.Content{
					Parts: []*genai.Part{
						{Text: "not a valid json object"},
					},
				},
			},
		},
	}

	schema := &llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"key": {Type: llm.SchemaPropertyTypeString},
		},
	}

	_, err := adapter.parseGenerateContentResponse(resp, genai.FinishReasonStop, schema)
	if err == nil {
		t.Fatal("expected schema error, got nil")
	}
}

func TestGoogleBatchAdapter_HTTPOperations(t *testing.T) {
	var cancelledBatchName string
	var uploadSessionStarted bool

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Incoming request: %s %s", r.Method, r.URL.Path)
		_, _ = io.ReadAll(r.Body)

		switch {
		// Batches.Create: POST /v1beta/models/{model}:batchGenerateContent
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":batchGenerateContent"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"name": "batches/job-123",
				"metadata": {
					"displayName": "batch-test-batch",
					"state": "BATCH_STATE_RUNNING"
				}
			}`))

		// Batches.Cancel: POST /v1beta/batches/...:cancel
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":cancel"):
			if strings.Contains(r.URL.Path, "already-cancelled") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error": {"code": 400, "message": "job is already cancelled", "status": "FAILED_PRECONDITION"}}`))
				return
			}
			cancelledBatchName = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		// Batches.Get for batches/job-123 (inline response)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/batches/job-123"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"name": "batches/job-123",
				"metadata": {
					"state": "BATCH_STATE_SUCCEEDED",
					"output": {
						"inlinedResponses": {
							"inlinedResponses": [
								{
									"response": {
										"candidates": [
											{
												"finishReason": "STOP",
												"content": {"parts": [{"text": "result 1"}]}
											}
										],
										"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 20}
									}
								},
								{
									"response": {
										"candidates": [
											{
												"finishReason": "MAX_TOKENS",
												"content": {"parts": [{"text": "truncated result"}]}
											}
										],
										"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 50}
									}
								}
							]
						}
					}
				}
			}`))

		// Batches.Get for batches/job-file (file-based response)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/batches/job-file"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"name": "batches/job-file",
				"metadata": {
					"state": "BATCH_STATE_SUCCEEDED",
					"output": {
						"responsesFile": "files/output-file-456"
					}
				}
			}`))

		// Files.Download for output-file-456
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/output-file-456"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Return JSONL content
			line1 := `{"key": "file-item-1", "response": {"candidates": [{"finishReason": "STOP", "content": {"parts": [{"text": "file result 1"}]}}], "usageMetadata": {"promptTokenCount": 15, "candidatesTokenCount": 25}}}`
			line2 := `{"key": "file-item-2", "response": {"candidates": [{"finishReason": "MAX_TOKENS", "content": {"parts": [{"text": "truncated file result"}]}}], "usageMetadata": {"promptTokenCount": 15, "candidatesTokenCount": 40}}}`
			_, _ = w.Write([]byte(line1 + "\n" + line2 + "\n"))

		// Files.Upload protocol:
		// Step 1: POST /upload/v1beta/files initiates upload session
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/v1beta/files"):
			uploadSessionStarted = true
			uploadURL := ts.URL + "/upload-session/session-123"
			w.Header().Set("X-Goog-Upload-URL", uploadURL)
			w.WriteHeader(http.StatusOK)

		// Step 2: POST /upload-session/session-123 completes file upload
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload-session/session-123"):
			cmd := r.Header.Get("X-Goog-Upload-Command")
			if strings.Contains(cmd, "finalize") {
				w.Header().Set("X-Goog-Upload-Status", "final")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"file": map[string]any{
						"name": "files/input-file-789",
						"uri":  "files/input-file-789",
					},
				})
			} else {
				w.Header().Set("X-Goog-Upload-Status", "active")
				w.WriteHeader(http.StatusOK)
			}

		// Batches.Get for batches/job-expired
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/batches/job-expired"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"name": "batches/job-expired",
				"metadata": {
					"state": "BATCH_STATE_EXPIRED"
				}
			}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:      "fake-key",
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: ts.URL},
	})
	if err != nil {
		t.Fatalf("failed to create genai client: %v", err)
	}

	adapter := NewGoogleAdapter(client, "gemini-2.5-flash", nil)

	// 1. Submit inline batch
	items := []*llm.BatchSubmitItem{
		{
			CustomID: "item-1",
			Chat: func() *llm.ChatContext {
				c := llm.NewChatContext()
				c.AddUserMessage("prompt 1")
				return c
			}(),
		},
		{
			CustomID: "item-2",
			Chat: func() *llm.ChatContext {
				c := llm.NewChatContext()
				c.AddUserMessage("prompt 2")
				return c
			}(),
		},
	}

	jobName, err := adapter.SubmitBatch(ctx, "test-batch", items)
	if err != nil {
		t.Fatalf("SubmitBatch failed: %v", err)
	}
	if jobName != "batches/job-123" {
		t.Fatalf("expected batches/job-123, got %s", jobName)
	}

	// 2. Poll inline batch
	status, err := adapter.GetBatch(ctx, "batches/job-123")
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}
	if status.State != "SUCCEEDED" {
		t.Fatalf("unexpected status: %+v", status)
	}

	// 3. Fetch inline results (one SUCCEEDED, one with MAX_TOKENS -> output_truncated)
	results, err := adapter.FetchBatchResults(ctx, "batches/job-123", items)
	if err != nil {
		t.Fatalf("FetchBatchResults failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Item 1: succeeded
	if results[0].CustomID != "item-1" || results[0].Response == nil || results[0].Response.Choices[0].Text != "result 1" {
		t.Fatalf("unexpected item 1 result: %+v", results[0])
	}
	// Item 2: output_truncated error
	if results[1].CustomID != "item-2" || results[1].Error == nil || results[1].Error.Reason != "output_truncated" {
		t.Fatalf("expected item 2 to fail with output_truncated, got %+v", results[1])
	}

	// 4. Cancel batch
	if err := adapter.CancelBatch(ctx, "batches/job-123"); err != nil {
		t.Fatalf("CancelBatch failed: %v", err)
	}
	if !strings.Contains(cancelledBatchName, "batches/job-123:cancel") {
		t.Fatalf("unexpected cancel path: %s", cancelledBatchName)
	}
	if err := adapter.CancelBatch(ctx, "batches/job-already-cancelled"); err != nil {
		t.Fatalf("CancelBatch idempotent failed: %v", err)
	}

	// 5. Expired batch
	expStatus, err := adapter.GetBatch(ctx, "batches/job-expired")
	if err != nil {
		t.Fatalf("GetBatch expired failed: %v", err)
	}
	if expStatus.State != "EXPIRED" {
		t.Fatalf("expected EXPIRED state, got %s", expStatus.State)
	}
	expResults, err := adapter.FetchBatchResults(ctx, "batches/job-expired", items)
	if err != nil {
		t.Fatalf("FetchBatchResults expired failed: %v", err)
	}
	for _, r := range expResults {
		if r.Error == nil || r.Error.Reason != "batch_expired" {
			t.Fatalf("expected batch_expired error on expired batch item, got %+v", r)
		}
	}

	// 6. File batch result download
	fileItems := []*llm.BatchSubmitItem{
		{
			CustomID: "file-item-1",
			Chat:     llm.NewChatContext(),
		},
		{
			CustomID: "file-item-2",
			Chat:     llm.NewChatContext(),
		},
	}
	fileResults, err := adapter.FetchBatchResults(ctx, "batches/job-file", fileItems)
	if err != nil {
		t.Fatalf("FetchBatchResults for file batch failed: %v", err)
	}
	if len(fileResults) != 2 {
		t.Fatalf("expected 2 file results, got %d", len(fileResults))
	}
	if fileResults[0].CustomID != "file-item-1" || fileResults[0].Response == nil || fileResults[0].Response.Choices[0].Text != "file result 1" {
		t.Fatalf("unexpected file item 1 result: %+v", fileResults[0])
	}
	if fileResults[1].CustomID != "file-item-2" || fileResults[1].Error == nil || fileResults[1].Error.Reason != "output_truncated" {
		t.Fatalf("expected file item 2 to fail with output_truncated, got %+v", fileResults[1])
	}

	// 7. Submit batch exceeding inline threshold (triggers Files.Upload)
	origThreshold := inlineBatchSizeThreshold
	inlineBatchSizeThreshold = 50 // small threshold for test
	defer func() { inlineBatchSizeThreshold = origThreshold }()

	smallFileChat := llm.NewChatContext()
	smallFileChat.AddUserMessage("this message exceeds 50 bytes threshold for file upload test")
	fileSubmitItems := []*llm.BatchSubmitItem{
		{
			CustomID: "file-upload-item-1",
			Chat:     smallFileChat,
		},
	}
	fileJobName, err := adapter.SubmitBatch(ctx, "file-upload-batch", fileSubmitItems)
	if err != nil {
		t.Fatalf("SubmitBatch file batch failed: %v", err)
	}
	if fileJobName != "batches/job-123" {
		t.Fatalf("unexpected file job name: %s", fileJobName)
	}
	if !uploadSessionStarted {
		t.Fatal("expected Files.Upload to have started for batch exceeding threshold")
	}
}
