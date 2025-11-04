package jsonrpc2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
)

// HTTPClient issues JSON-RPC requests over HTTP.
type HTTPClient struct {
	endpoint string
	client   *http.Client
	nextID   uint64
}

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

// Call sends a JSON-RPC request with a generated identifier and returns the server response.
func (c *HTTPClient) Call(ctx context.Context, method string, params any) (*Response, error) {
	if c == nil {
		return nil, fmt.Errorf("jsonrpc2: HTTPClient is nil")
	}

	id := atomic.AddUint64(&c.nextID, 1)
	idStr := strconv.FormatUint(id, 10)

	req := &Request{
		JSONRPC: Version,
		Method:  method,
		ID:      idStr,
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req.Params = raw
	}

	body, status, err := c.send(ctx, req)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jsonrpc2: unexpected HTTP status %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("jsonrpc2: empty response body")
	}

	var resp Response
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, err
	}
	if resp.JSONRPC != Version {
		return nil, fmt.Errorf("jsonrpc2: invalid JSON-RPC version %q", resp.JSONRPC)
	}

	return &resp, nil
}

// Notify sends a JSON-RPC notification without waiting for a server response.
func (c *HTTPClient) Notify(ctx context.Context, method string, params any) error {
	if c == nil {
		return fmt.Errorf("jsonrpc2: HTTPClient is nil")
	}

	req := &Request{
		JSONRPC: Version,
		Method:  method,
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}

	body, status, err := c.send(ctx, req)
	if err != nil {
		return err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return fmt.Errorf("jsonrpc2: unexpected HTTP status %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}

	var resp Response
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return fmt.Errorf("jsonrpc2: cannot decode notification response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

// Batch issues a JSON-RPC batch request and returns the responses in the same order as calls.
// Notifications yield nil entries in the returned slice.
func (c *HTTPClient) Batch(ctx context.Context, calls []BatchCall) ([]*Response, error) {
	if c == nil {
		return nil, fmt.Errorf("jsonrpc2: HTTPClient is nil")
	}
	if len(calls) == 0 {
		return nil, nil
	}

	requests := make([]Request, 0, len(calls))
	responses := make([]*Response, len(calls))
	idToIndex := make(map[string]int, len(calls))

	for i, call := range calls {
		req := Request{
			JSONRPC: Version,
			Method:  call.Method,
		}
		if call.Params != nil {
			raw, err := json.Marshal(call.Params)
			if err != nil {
				return nil, err
			}
			req.Params = raw
		}
		if !call.Notify {
			id := atomic.AddUint64(&c.nextID, 1)
			idStr := strconv.FormatUint(id, 10)
			req.ID = idStr
			idToIndex[idStr] = i
		}
		requests = append(requests, req)
	}

	body, status, err := c.send(ctx, requests)
	if err != nil {
		return nil, err
	}
	if len(idToIndex) == 0 {
		// All notifications; servers may legitimately return no content.
		return responses, nil
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jsonrpc2: unexpected HTTP status %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("jsonrpc2: empty batch response body")
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
		// Some servers may return a single response object when only one call was present.
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
		responses[idx] = &resp
	default:
		return nil, fmt.Errorf("jsonrpc2: invalid batch response payload")
	}

	for _, idx := range idToIndex {
		if responses[idx] == nil {
			return nil, fmt.Errorf("jsonrpc2: missing response for call index %d", idx)
		}
	}

	return responses, nil
}

func (c *HTTPClient) send(ctx context.Context, payload any) ([]byte, int, error) {
	data, err := json.Marshal(payload)
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
