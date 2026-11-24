package adapters

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
)

func TestTranslateHTTPErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"pair", 400, `{"error":{"code":"unsupported_language_pair","message":"x"}}`, func(e error) bool { return errors.Is(e, llm.ErrUnsupportedLanguagePair) }},
		{"no route", 404, `not found`, func(e error) bool { return errors.Is(e, llm.ErrTranslationUnsupported) }},
		{"overloaded", 503, `busy`, llm.IsRetryableError},
		{"late failure", 200, ` {"error":{"code":"engine_error","message":"oom"}}`, llm.IsRetryableError},
		{"bad request", 400, `{"error":{"code":"bad_request","message":"empty"}}`, func(e error) bool { return !llm.IsRetryableError(e) }},
		{"short", 200, `{"translations":[]}`, llm.ShouldSkipToNextModel},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		_, err := TranslateHTTP(context.Background(), srv.Client(), srv.URL+"/v1", "", "m", &llm.TranslateRequest{Texts: []string{"a"}, TargetLanguage: "de"})
		srv.Close()
		if err == nil || !c.check(err) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}
