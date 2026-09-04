package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// doctorClient is a small HTTP helper for the `garess doctor` command.
type doctorClient struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func newDoctorClient(endpoint, apiKey string) *doctorClient {
	return &doctorClient{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		apiKey:   apiKey,
		client:   &http.Client{},
	}
}

func (c *doctorClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// Ping verifies the endpoint is reachable by listing models.
func Ping(ctx context.Context, endpoint, apiKey string) error {
	c := newDoctorClient(endpoint, apiKey)
	if _, err := c.get(ctx, "/models"); err != nil {
		return fmt.Errorf("endpoint unreachable: %w", err)
	}
	return nil
}

// ListModels returns the model IDs advertised by the endpoint.
func ListModels(ctx context.Context, endpoint, apiKey string) ([]string, error) {
	infos, err := fetchModels(ctx, endpoint, apiKey)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(infos))
	for _, m := range infos {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// contextWindowKeys are metadata keys OpenAI-compatible servers (llama.cpp,
// vLLM, LM Studio, ...) use to advertise a model's context length. Looked up
// at the top level of a /v1/models entry and inside its nested `meta` object.
var contextWindowKeys = []string{
	"context_length",
	"context_window",
	"max_context_length",
	"max_model_len",
	"max_sequence_length",
	"n_ctx_train",
	"n_ctx",
}

// LookupContextWindow returns the context size in tokens advertised by the
// endpoint for modelName, or 0 with an error when the endpoint does not
// advertise one. The exact model match wins; otherwise the first model entry
// that reports a context size is used. Best effort — callers fall back to a
// configured value and then a default.
func LookupContextWindow(ctx context.Context, endpoint, apiKey, modelName string) (int, error) {
	infos, err := fetchModels(ctx, endpoint, apiKey)
	if err != nil {
		return 0, err
	}
	var fallback int
	for _, m := range infos {
		w := contextWindowFromMeta(m.Meta)
		if w <= 0 {
			continue
		}
		if fallback == 0 {
			fallback = w
		}
		if m.ID == modelName {
			return w, nil
		}
	}
	if fallback > 0 {
		return fallback, nil
	}
	return 0, fmt.Errorf("endpoint does not advertise a context length for %q", modelName)
}

// modelInfo is one GET /v1/models entry with its metadata flattened: every
// top-level key plus any keys nested under `meta` (which wins on conflicts).
type modelInfo struct {
	ID   string
	Meta map[string]any
}

// fetchModels loads and flattens GET /v1/models entries.
func fetchModels(ctx context.Context, endpoint, apiKey string) ([]modelInfo, error) {
	c := newDoctorClient(endpoint, apiKey)
	body, err := c.get(ctx, "/models")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse /models: %w", err)
	}
	infos := make([]modelInfo, 0, len(out.Data))
	for _, e := range out.Data {
		id, _ := e["id"].(string)
		meta := e
		if sub, ok := e["meta"].(map[string]any); ok {
			merged := make(map[string]any, len(e)+len(sub))
			for k, v := range e {
				merged[k] = v
			}
			for k, v := range sub {
				merged[k] = v // nested meta wins over the same-named top key
			}
			meta = merged
		}
		infos = append(infos, modelInfo{ID: id, Meta: meta})
	}
	return infos, nil
}

// contextWindowFromMeta extracts a token count from one of
// contextWindowKeys. JSON numbers decode as float64; some servers advertise
// string numbers ("32768"), which are accepted too.
func contextWindowFromMeta(meta map[string]any) int {
	for _, k := range contextWindowKeys {
		v, ok := meta[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n > 0 && n == float64(int(n)) {
				return int(n)
			}
		case int:
			if n > 0 {
				return n
			}
		case json.Number:
			if i, err := n.Int64(); err == nil && i > 0 {
				return int(i)
			}
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil && i > 0 {
				return i
			}
		}
	}
	return 0
}
