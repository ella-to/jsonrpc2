package jsonrpc2_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ella.to/jsonrpc2"
)

func TestHTTPClientCall(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 1)

	handler := jsonrpc2.NewHTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		return &jsonrpc2.Response{Result: mustRaw(t, map[string]string{"message": "pong"})}, nil
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := jsonrpc2.NewHTTPClient(srv.URL)

	resp, err := client.Call(context.Background(), "echo", map[string]string{"message": "ping"})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if resp == nil {
		t.Fatalf("Call returned nil response")
	}
	if resp.Error != nil {
		t.Fatalf("Call returned JSON-RPC error: %+v", resp.Error)
	}

	received := recv(t, reqCh)
	if received.Method != "echo" {
		t.Fatalf("unexpected method: %s", received.Method)
	}
	if received.ID == nil {
		t.Fatalf("expected request ID to be set")
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result["message"] != "pong" {
		t.Fatalf("unexpected result payload: %+v", result)
	}
}

func TestHTTPClientNotify(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 1)

	handler := jsonrpc2.NewHTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		return nil, nil
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := jsonrpc2.NewHTTPClient(srv.URL)

	if err := client.Notify(context.Background(), "event", map[string]int{"value": 42}); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}

	received := recv(t, reqCh)
	if received.Method != "event" {
		t.Fatalf("unexpected method: %s", received.Method)
	}
	if received.ID != nil {
		t.Fatalf("notification should not include an ID")
	}
}

func TestHTTPClientBatch(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 3)

	handler := jsonrpc2.NewHTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		if req.Method == "error" {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.InternalError, Message: "boom"}
		}
		return &jsonrpc2.Response{Result: mustRaw(t, map[string]any{"method": req.Method})}, nil
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := jsonrpc2.NewHTTPClient(srv.URL)

	calls := []jsonrpc2.BatchCall{
		{Method: "first", Params: map[string]int{"value": 1}},
		{Method: "notify", Notify: true},
		{Method: "error"},
	}

	responses, err := client.Batch(context.Background(), calls)
	if err != nil {
		t.Fatalf("Batch returned error: %v", err)
	}
	if len(responses) != len(calls) {
		t.Fatalf("expected %d responses, got %d", len(calls), len(responses))
	}

	if responses[0] == nil || responses[0].Error != nil {
		t.Fatalf("expected success response for first call: %+v", responses[0])
	}
	var firstPayload map[string]any
	if err := json.Unmarshal(responses[0].Result, &firstPayload); err != nil {
		t.Fatalf("failed to unmarshal first payload: %v", err)
	}
	if firstPayload["method"] != "first" {
		t.Fatalf("unexpected first payload: %+v", firstPayload)
	}

	if responses[1] != nil {
		t.Fatalf("expected nil entry for notification, got %+v", responses[1])
	}

	if responses[2] == nil || responses[2].Error == nil {
		t.Fatalf("expected error response for third call: %+v", responses[2])
	}
	if responses[2].Error.Code != jsonrpc2.InternalError {
		t.Fatalf("unexpected error code: %d", responses[2].Error.Code)
	}

	received := map[string]struct{}{}
	for range 3 {
		req := recv(t, reqCh)
		received[req.Method] = struct{}{}
	}
	if len(received) != 3 {
		t.Fatalf("expected three dispatched methods, got %+v", received)
	}
}

func TestHTTPHandlerParseError(t *testing.T) {
	handler := jsonrpc2.NewHTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		return &jsonrpc2.Response{Result: mustRaw(t, true)}, nil
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", bytes.NewBufferString("{"))
	if err != nil {
		t.Fatalf("failed to POST invalid payload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	var rpcResp jsonrpc2.Response
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if rpcResp.Error == nil {
		t.Fatalf("expected JSON-RPC error in response")
	}
	if rpcResp.Error.Code != jsonrpc2.ParseError {
		t.Fatalf("unexpected error code: %d", rpcResp.Error.Code)
	}
	if rpcResp.ID != nil {
		t.Fatalf("expected null id for parse error, got %v", rpcResp.ID)
	}
}
