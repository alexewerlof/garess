package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelsServer serves a canned GET /v1/models body.
func modelsServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLookupContextWindowExactMatch(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "other", "context_length": 8192},
	    {"id": "gemma", "meta": {"context_length": 65536}}
	  ]
	}`)
	got, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "gemma")
	if err != nil {
		t.Fatal(err)
	}
	if got != 65536 {
		t.Fatalf("window = %d, want 65536", got)
	}
}

func TestLookupContextWindowTopLevelKeys(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "m", "max_model_len": 131072}
	  ]
	}`)
	got, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if got != 131072 {
		t.Fatalf("window = %d, want 131072", got)
	}
}

func TestLookupContextWindowFallsBackToFirstReported(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "alpha", "context_window": 16384},
	    {"id": "beta"}
	  ]
	}`)
	// "beta" reports nothing: fall back to the first entry that does.
	got, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if got != 16384 {
		t.Fatalf("window = %d, want 16384", got)
	}
}

func TestLookupContextWindowStringNumber(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "m", "n_ctx": "4096"}
	  ]
	}`)
	got, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if got != 4096 {
		t.Fatalf("window = %d, want 4096", got)
	}
}

func TestLookupContextWindowNotAdvertised(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "m"}
	  ]
	}`)
	if _, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "m"); err == nil {
		t.Fatal("expected error when no context length is advertised")
	}
}

func TestLookupContextWindowRejectsNegativeValues(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "m", "context_length": -5}
	  ]
	}`)
	if _, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "m"); err == nil {
		t.Fatal("expected error for non-positive context length")
	}
}

func TestListModelsStillParsesPlainIDs(t *testing.T) {
	srv := modelsServer(t, `{
	  "data": [
	    {"id": "one", "object": "model"},
	    {"id": "two", "context_length": 32768}
	  ]
	}`)
	ids, err := ListModels(context.Background(), srv.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "one" || ids[1] != "two" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestLookupContextWindowEndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, err := LookupContextWindow(context.Background(), srv.URL+"/v1", "", "m")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want server error surfaced", err)
	}
}
