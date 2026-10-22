package cleanup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The API key travels in the x-goog-api-key header, never in the URL: Go puts the
// request URL into transport errors, which the server logs.
func TestCallOnceSendsKeyInHeader(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotQuery = r.Header.Get("x-goog-api-key"), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer srv.Close()
	if _, err := callOnce(context.Background(), "SECRET123", srv.URL, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if gotKey != "SECRET123" || gotQuery != "" {
		t.Fatalf("header=%q query=%q", gotKey, gotQuery)
	}
}

func TestCallOnceErrorOmitsKey(t *testing.T) {
	_, err := callOnce(context.Background(), "SECRET123", "http://127.0.0.1:1/x", []byte("{}"))
	if err == nil || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiURLHasNoKey(t *testing.T) {
	if u := geminiURL("gemma-4-31b-it"); strings.Contains(u, "key=") {
		t.Fatalf("url carries key: %s", u)
	}
}
