package jsonrpc2

import (
	"context"
	"encoding/json"
	"fmt"
)

// Version represents the JSON-RPC protocol version supported by this package.
const Version = "2.0"

// Standard error codes as defined in the JSON-RPC 2.0 specification.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Request represents a JSON-RPC 2.0 request message.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id,omitempty"`
}

// Response represents a JSON-RPC 2.0 response message.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      any             `json:"id"`
}

// Error represents a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface for JSON-RPC error objects.
func (e *Error) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// Handler serves JSON-RPC requests.
type Handler interface {
	Handle(ctx context.Context, req *Request) (*Response, error)
}

// HandlerFunc adapts a function to the Handler interface.
type HandlerFunc func(ctx context.Context, req *Request) (*Response, error)

// Handle dispatches the request to f.
func (f HandlerFunc) Handle(ctx context.Context, req *Request) (*Response, error) {
	return f(ctx, req)
}

func WithRequest(method string, params any, isNotify bool) *Request {
	req := &Request{
		JSONRPC: Version,
		Method:  method,
	}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			panic(err)
		}
		req.Params = raw
	}

	if !isNotify {
		req.ID = "1" // Dummy ID for non-notification requests
	}

	return req
}

type Caller interface {
	Call(ctx context.Context, requests ...*Request) ([]*Response, error)
}
