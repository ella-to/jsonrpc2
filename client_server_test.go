package jsonrpc2_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"ella.to/jsonrpc2"
)

type serverHarness struct {
	client *jsonrpc2.Client
	done   chan error
	cancel context.CancelFunc
}

type rawServerHarness struct {
	conn   net.Conn
	server *jsonrpc2.Server
	done   chan error
	cancel context.CancelFunc
}

func newServerHarness(t *testing.T, handler jsonrpc2.Handler) *serverHarness {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	server := jsonrpc2.NewServer(serverConn, handler)
	client := jsonrpc2.NewClient(clientConn)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- server.Serve(ctx)
	}()

	h := &serverHarness{client: client, done: done, cancel: cancel}

	t.Cleanup(func() {
		_ = h.client.Close()
		_ = server.Close()
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				t.Fatalf("server exited with error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("server shutdown timed out")
		}
	})

	return h
}

func newRawServerHarness(t *testing.T, handler jsonrpc2.Handler) *rawServerHarness {
	t.Helper()

	serverConn, clientConn := net.Pipe()

	server := jsonrpc2.NewServer(serverConn, handler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- server.Serve(ctx)
	}()

	h := &rawServerHarness{conn: clientConn, server: server, done: done, cancel: cancel}

	t.Cleanup(func() {
		_ = h.conn.Close()
		_ = server.Close()
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				t.Fatalf("server exited with error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("server shutdown timed out")
		}
	})

	return h
}

func TestClientCallSuccess(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 1)

	h := newServerHarness(t, jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		return &jsonrpc2.Response{Result: mustRaw(t, map[string]string{"message": "pong"})}, nil
	}))

	resp, err := h.client.Call(context.Background(), "echo", map[string]string{"message": "ping"})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if resp == nil {
		t.Fatalf("Call returned nil response")
	}
	if resp.Error != nil {
		t.Fatalf("Call returned JSON-RPC error: %+v", resp.Error)
	}
	if resp.JSONRPC != jsonrpc2.Version {
		t.Fatalf("unexpected JSON-RPC version: %s", resp.JSONRPC)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("failed to decode result: %v", err)
	}
	if result["message"] != "pong" {
		t.Fatalf("unexpected result payload: %+v", result)
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

func TestClientNotify(t *testing.T) {
	notifyCh := make(chan jsonrpc2.Request, 1)

	h := newServerHarness(t, jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		notifyCh <- *req
		return nil, nil
	}))

	if err := h.client.Notify(context.Background(), "event", map[string]int{"value": 42}); err != nil {
		t.Fatalf("Notify returned error: %v", err)
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

func TestClientBatch(t *testing.T) {
	reqCh := make(chan jsonrpc2.Request, 3)

	h := newServerHarness(t, jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		reqCh <- *req
		if req.Method == "error" {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.InternalError, Message: "error"}
		}
		return &jsonrpc2.Response{Result: mustRaw(t, map[string]any{"method": req.Method})}, nil
	}))

	calls := []jsonrpc2.BatchCall{
		{Method: "first", Params: map[string]int{"value": 1}},
		{Method: "notify", Params: map[string]int{"value": 2}, Notify: true},
		{Method: "error"},
	}

	responses, err := h.client.Batch(context.Background(), calls)
	if err != nil {
		t.Fatalf("Batch returned error: %v", err)
	}
	if len(responses) != len(calls) {
		t.Fatalf("expected %d responses, got %d", len(calls), len(responses))
	}

	if responses[0] == nil {
		t.Fatalf("expected response for first call")
	}
	if responses[0].Error != nil {
		t.Fatalf("unexpected error for first call: %+v", responses[0].Error)
	}
	var firstPayload map[string]any
	if err := json.Unmarshal(responses[0].Result, &firstPayload); err != nil {
		t.Fatalf("failed to decode first result: %v", err)
	}
	if firstPayload["method"] != "first" {
		t.Fatalf("unexpected payload for first response: %+v", firstPayload)
	}

	if responses[1] != nil {
		t.Fatalf("expected nil entry for notification, got %+v", responses[1])
	}

	if responses[2] == nil {
		t.Fatalf("expected error response for third call")
	}
	if responses[2].Error == nil {
		t.Fatalf("expected JSON-RPC error in third response")
	}
	if responses[2].Error.Code != jsonrpc2.InternalError {
		t.Fatalf("unexpected error code: %d", responses[2].Error.Code)
	}
	if responses[2].Error.Message != "error" {
		t.Fatalf("unexpected error message: %s", responses[2].Error.Message)
	}

	received := map[string]struct{}{}
	for i := 0; i < 3; i++ {
		req := recv(t, reqCh)
		received[req.Method] = struct{}{}
		if req.Method == "notify" {
			if req.ID != nil {
				t.Fatalf("notification should not include ID, got %v", req.ID)
			}
		} else {
			if req.ID == nil {
				t.Fatalf("expected ID for method %s", req.Method)
			}
		}
	}
	if len(received) != 3 {
		t.Fatalf("missing dispatched methods: %+v", received)
	}
}

func TestClientCallErrors(t *testing.T) {
	h := newServerHarness(t, jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		switch req.Method {
		case "missing":
			return nil, &jsonrpc2.Error{Code: jsonrpc2.MethodNotFound, Message: "missing"}
		case "explode":
			return nil, errors.New("boom")
		default:
			return &jsonrpc2.Response{Result: mustRaw(t, true)}, nil
		}
	}))

	resp, err := h.client.Call(context.Background(), "missing", nil)
	if err != nil {
		t.Fatalf("Call returned transport error: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error response")
	}
	if resp.Error.Code != jsonrpc2.MethodNotFound {
		t.Fatalf("unexpected error code: %d", resp.Error.Code)
	}
	if resp.Error.Message != "missing" {
		t.Fatalf("unexpected error message: %s", resp.Error.Message)
	}

	resp, err = h.client.Call(context.Background(), "explode", nil)
	if err != nil {
		t.Fatalf("Call returned transport error: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error response")
	}
	if resp.Error.Code != jsonrpc2.InternalError {
		t.Fatalf("unexpected error code: %d", resp.Error.Code)
	}
	if resp.Error.Message != "boom" {
		t.Fatalf("unexpected error message: %s", resp.Error.Message)
	}
}

func TestServerHandlesBatchRequests(t *testing.T) {
	callCh := make(chan string, 3)

	h := newRawServerHarness(t, jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		callCh <- req.Method
		return &jsonrpc2.Response{Result: mustRaw(t, map[string]any{"method": req.Method})}, nil
	}))

	encoder := json.NewEncoder(h.conn)
	decoder := json.NewDecoder(h.conn)
	decoder.UseNumber()

	batch := []jsonrpc2.Request{
		{JSONRPC: jsonrpc2.Version, Method: "one", Params: mustRaw(t, map[string]int{"value": 1}), ID: "call-1"},
		{JSONRPC: jsonrpc2.Version, Method: "notify", Params: mustRaw(t, map[string]int{"value": 2})},
		{JSONRPC: jsonrpc2.Version, Method: "two", Params: mustRaw(t, map[string]int{"value": 3}), ID: 2},
	}

	if err := encoder.Encode(batch); err != nil {
		t.Fatalf("failed to encode batch: %v", err)
	}

	var responses []jsonrpc2.Response
	if err := decoder.Decode(&responses); err != nil {
		t.Fatalf("failed to decode batch response: %v", err)
	}

	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	if responses[0].JSONRPC != jsonrpc2.Version {
		t.Fatalf("unexpected version in first response: %s", responses[0].JSONRPC)
	}
	if responses[0].ID != "call-1" {
		t.Fatalf("unexpected id in first response: %v", responses[0].ID)
	}
	if responses[0].Error != nil {
		t.Fatalf("unexpected error in first response: %+v", responses[0].Error)
	}

	var firstPayload map[string]any
	if err := json.Unmarshal(responses[0].Result, &firstPayload); err != nil {
		t.Fatalf("failed to decode first payload: %v", err)
	}
	if firstPayload["method"] != "one" {
		t.Fatalf("unexpected payload for first response: %+v", firstPayload)
	}

	if responses[1].JSONRPC != jsonrpc2.Version {
		t.Fatalf("unexpected version in second response: %s", responses[1].JSONRPC)
	}
	if responses[1].Error != nil {
		t.Fatalf("unexpected error in second response: %+v", responses[1].Error)
	}
	idNumber, ok := responses[1].ID.(json.Number)
	if !ok {
		t.Fatalf("expected numeric id in second response, got %T", responses[1].ID)
	}
	idValue, err := idNumber.Int64()
	if err != nil {
		t.Fatalf("failed to decode numeric id: %v", err)
	}
	if idValue != 2 {
		t.Fatalf("unexpected id in second response: %d", idValue)
	}

	var secondPayload map[string]any
	if err := json.Unmarshal(responses[1].Result, &secondPayload); err != nil {
		t.Fatalf("failed to decode second payload: %v", err)
	}
	if secondPayload["method"] != "two" {
		t.Fatalf("unexpected payload for second response: %+v", secondPayload)
	}

	methods := map[string]struct{}{
		"one":    {},
		"two":    {},
		"notify": {},
	}
	for i := 0; i < 3; i++ {
		m := recv(t, callCh)
		if _, ok := methods[m]; !ok {
			t.Fatalf("unexpected method dispatched: %s", m)
		}
		delete(methods, m)
	}
	if len(methods) != 0 {
		t.Fatalf("missing dispatched methods: %+v", methods)
	}
}

func TestServerHandlesBatchWithInvalidEntries(t *testing.T) {
	h := newRawServerHarness(t, jsonrpc2.HandlerFunc(func(ctx context.Context, req *jsonrpc2.Request) (*jsonrpc2.Response, error) {
		return &jsonrpc2.Response{Result: mustRaw(t, req.Method)}, nil
	}))

	encoder := json.NewEncoder(h.conn)
	decoder := json.NewDecoder(h.conn)
	decoder.UseNumber()

	batch := []any{
		jsonrpc2.Request{JSONRPC: jsonrpc2.Version, Method: "ok", ID: 1},
		"bad-entry",
	}

	if err := encoder.Encode(batch); err != nil {
		t.Fatalf("failed to encode batch: %v", err)
	}

	var responses []jsonrpc2.Response
	if err := decoder.Decode(&responses); err != nil {
		t.Fatalf("failed to decode batch response: %v", err)
	}

	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}

	firstID, ok := responses[0].ID.(json.Number)
	if !ok {
		t.Fatalf("expected numeric id in first response, got %T", responses[0].ID)
	}
	if val, err := firstID.Int64(); err != nil || val != 1 {
		t.Fatalf("unexpected id in first response: %v (%v)", firstID, err)
	}
	if responses[0].Error != nil {
		t.Fatalf("unexpected error in first response: %+v", responses[0].Error)
	}

	if responses[1].ID != nil {
		t.Fatalf("expected null id in error response, got %v", responses[1].ID)
	}
	if responses[1].Error == nil {
		t.Fatalf("expected error payload for invalid entry")
	}
	if responses[1].Error.Code != jsonrpc2.InvalidRequest {
		t.Fatalf("unexpected error code: %d", responses[1].Error.Code)
	}
	if responses[1].Error.Message != "invalid request" {
		t.Fatalf("unexpected error message: %s", responses[1].Error.Message)
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal value: %v", err)
	}
	return json.RawMessage(b)
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for value")
	}
	var zero T
	return zero
}
