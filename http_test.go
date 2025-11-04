package jsonrpc2_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ella.to/jsonrpc2"
)

func TestHTTPClientCall(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 1)

	handler := jsonrpc2.HTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		return &jsonrpc2.Response{Result: mustRaw(t, map[string]string{"message": "pong"})}, nil
	}))

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := jsonrpc2.NewHTTPClient(server.URL, server.Client())

	responses, err := client.Call(context.Background(), jsonrpc2.WithRequest("echo", map[string]string{"message": "ping"}, false))
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if len(responses) != 1 {
		t.Fatalf("expected single response, got %d", len(responses))
	}
	resp := responses[0]
	if resp == nil {
		t.Fatalf("Call returned nil response")
	}
	if resp.JSONRPC != jsonrpc2.Version {
		t.Fatalf("unexpected JSON-RPC version: %s", resp.JSONRPC)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}

	var payload map[string]string
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("failed to decode result: %v", err)
	}
	if payload["message"] != "pong" {
		t.Fatalf("unexpected response payload: %+v", payload)
	}

	received := recv(t, reqCh)
	if received.Method != "echo" {
		t.Fatalf("unexpected method: %s", received.Method)
	}
	if received.ID == nil {
		t.Fatalf("expected request ID to be set")
	}

	var params map[string]string
	if err := json.Unmarshal(received.Params, &params); err != nil {
		t.Fatalf("failed to decode params: %v", err)
	}
	if params["message"] != "ping" {
		t.Fatalf("unexpected params payload: %+v", params)
	}
}

func TestHTTPClientNotify(t *testing.T) {
	notifyCh := make(chan jsonrpc2.Request, 1)

	handler := jsonrpc2.HTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		notifyCh <- *req
		return nil, nil
	}))

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := jsonrpc2.NewHTTPClient(server.URL, server.Client())

	responses, err := client.Call(context.Background(), jsonrpc2.WithRequest("event", map[string]int{"value": 42}, true))
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if len(responses) != 1 {
		t.Fatalf("expected single response entry, got %d", len(responses))
	}
	if responses[0] != nil {
		t.Fatalf("expected nil response for notification, got %+v", responses[0])
	}

	received := recv(t, notifyCh)
	if received.Method != "event" {
		t.Fatalf("unexpected method: %s", received.Method)
	}
	if received.ID != nil {
		t.Fatalf("notification should not include an ID")
	}

	var params map[string]int
	if err := json.Unmarshal(received.Params, &params); err != nil {
		t.Fatalf("failed to decode params: %v", err)
	}
	if params["value"] != 42 {
		t.Fatalf("unexpected params payload: %+v", params)
	}
}

func TestHTTPClientBatch(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 4)

	handler := jsonrpc2.HTTPHandler(jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		switch req.Method {
		case "first", "second":
			return &jsonrpc2.Response{Result: mustRaw(t, map[string]any{"method": req.Method})}, nil
		case "failure":
			return nil, &jsonrpc2.Error{Code: jsonrpc2.InternalError, Message: "boom"}
		default:
			return nil, nil
		}
	}))

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := jsonrpc2.NewHTTPClient(server.URL, server.Client())

	requests := []*jsonrpc2.Request{
		jsonrpc2.WithRequest("first", map[string]int{"value": 1}, false),
		jsonrpc2.WithRequest("notify", map[string]int{"value": 2}, true),
		jsonrpc2.WithRequest("failure", nil, false),
		jsonrpc2.WithRequest("second", map[string]int{"value": 3}, false),
	}

	responses, err := client.Call(context.Background(), requests...)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if len(responses) != len(requests) {
		t.Fatalf("expected %d responses, got %d", len(requests), len(responses))
	}

	if responses[0] == nil || responses[0].Error != nil {
		t.Fatalf("unexpected response for first call: %+v", responses[0])
	}
	var firstPayload map[string]any
	if err := json.Unmarshal(responses[0].Result, &firstPayload); err != nil {
		t.Fatalf("failed to decode first payload: %v", err)
	}
	if firstPayload["method"] != "first" {
		t.Fatalf("unexpected payload for first response: %+v", firstPayload)
	}

	if responses[1] != nil {
		t.Fatalf("expected nil entry for notification, got %+v", responses[1])
	}

	if responses[2] == nil || responses[2].Error == nil {
		t.Fatalf("expected error response for failure call, got %+v", responses[2])
	}
	if responses[2].Error.Code != jsonrpc2.InternalError {
		t.Fatalf("unexpected error code: %d", responses[2].Error.Code)
	}
	if responses[2].Error.Message != "boom" {
		t.Fatalf("unexpected error message: %s", responses[2].Error.Message)
	}

	if responses[3] == nil || responses[3].Error != nil {
		t.Fatalf("unexpected response for second call: %+v", responses[3])
	}
	var secondPayload map[string]any
	if err := json.Unmarshal(responses[3].Result, &secondPayload); err != nil {
		t.Fatalf("failed to decode second payload: %v", err)
	}
	if secondPayload["method"] != "second" {
		t.Fatalf("unexpected payload for second response: %+v", secondPayload)
	}

	methods := map[string]struct{}{
		"first":   {},
		"second":  {},
		"notify":  {},
		"failure": {},
	}
	for i := 0; i < len(requests); i++ {
		req := recv(t, reqCh)
		delete(methods, req.Method)
		if req.Method == "notify" && req.ID != nil {
			t.Fatalf("expected notification without ID, got %v", req.ID)
		}
		if req.Method != "notify" && req.ID == nil {
			t.Fatalf("expected ID for call %s", req.Method)
		}
	}
	if len(methods) != 0 {
		t.Fatalf("missing dispatched methods: %+v", methods)
	}
}
