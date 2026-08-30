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
	c := newDoctorClient(endpoint, apiKey)
	body, err := c.get(ctx, "/models")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse /models: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
