package jsonrpc2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HTTPClient issues JSON-RPC requests over HTTP.
type HTTPClient struct {
	endpoint string
	client   *http.Client
}

var _ Caller = (*HTTPClient)(nil)

// NewHTTPClient constructs an HTTP JSON-RPC client targeting endpoint. If client is nil,
// http.DefaultClient is used.
func NewHTTPClient(endpoint string, client *http.Client) *HTTPClient {
	if endpoint == "" {
		panic("jsonrpc2: endpoint cannot be empty")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPClient{
		endpoint: endpoint,
		client:   client,
	}
}

// Call sends the provided requests to the JSON-RPC endpoint. The payload is always
// encoded as a JSON array, even when only a single request is supplied. Responses are
// aligned with the order of the supplied requests, and notifications (requests without
// an id) yield nil entries in the returned slice.
func (c *HTTPClient) Call(ctx context.Context, requests ...*Request) ([]*Response, error) {
	if c == nil {
		return nil, fmt.Errorf("jsonrpc2: HTTPClient is nil")
	}
	if len(requests) == 0 {
		return nil, nil
	}

	responses := make([]*Response, len(requests))
	idToIndex := make(map[string]int, len(requests))
	expected := 0

	for i, req := range requests {
		if req == nil {
			return nil, fmt.Errorf("jsonrpc2: request at index %d is nil", i)
		}
		if req.JSONRPC == "" {
			req.JSONRPC = Version
		}
		if req.ID == nil {
			continue
		}
		idKey, ok := normalizeID(req.ID)
		if !ok {
			return nil, fmt.Errorf("jsonrpc2: invalid request id at index %d", i)
		}
		if _, exists := idToIndex[idKey]; exists {
			return nil, fmt.Errorf("jsonrpc2: duplicate request id %s", idKey)
		}
		idToIndex[idKey] = i
		expected++
	}

	body, status, err := c.send(ctx, requests)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jsonrpc2: unexpected HTTP status %d", status)
	}
	if expected == 0 {
		return responses, nil
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("jsonrpc2: empty response body")
	}

	switch trimmed[0] {
	case '[':
		var payload []Response
		if err := json.Unmarshal(trimmed, &payload); err != nil {
			return nil, err
		}
		for i := range payload {
			resp := payload[i]
			if resp.JSONRPC != Version {
				return nil, fmt.Errorf("jsonrpc2: invalid JSON-RPC version %q", resp.JSONRPC)
			}
			idKey, ok := normalizeID(resp.ID)
			if !ok {
				continue
			}
			idx, ok := idToIndex[idKey]
			if !ok {
				continue
			}
			respCopy := resp
			responses[idx] = &respCopy
		}
	case '{':
		var resp Response
		if err := json.Unmarshal(trimmed, &resp); err != nil {
			return nil, err
		}
		if resp.JSONRPC != Version {
			return nil, fmt.Errorf("jsonrpc2: invalid JSON-RPC version %q", resp.JSONRPC)
		}
		idKey, ok := normalizeID(resp.ID)
		if !ok {
			return nil, fmt.Errorf("jsonrpc2: response missing id")
		}
		idx, ok := idToIndex[idKey]
		if !ok {
			return nil, fmt.Errorf("jsonrpc2: unexpected response id %s", idKey)
		}
		respCopy := resp
		responses[idx] = &respCopy
	default:
		return nil, fmt.Errorf("jsonrpc2: invalid response payload")
	}

	for _, idx := range idToIndex {
		if responses[idx] == nil {
			return nil, fmt.Errorf("jsonrpc2: missing response for request index %d", idx)
		}
	}

	return responses, nil
}

func (c *HTTPClient) send(ctx context.Context, requests []*Request) ([]byte, int, error) {
	data, err := json.Marshal(requests)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return body, resp.StatusCode, nil
}
