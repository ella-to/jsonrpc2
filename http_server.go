package jsonrpc2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// HTTPHandler adapts a JSON-RPC Handler to a net/http.Handler.
func HTTPHandler(handler Handler) http.Handler {
	if handler == nil {
		panic("jsonrpc2: handler cannot be nil")
	}
	return &httpServer{handler: handler}
}

type httpServer struct {
	handler Handler
}

func (s *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, s.errorResponseWithNull(ParseError, "failed to read request body", err.Error()), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		s.writeJSON(w, s.errorResponseWithNull(InvalidRequest, "invalid request", "empty payload"))
		return
	}

	ctx := r.Context()

	if trimmed[0] == '[' {
		s.handleBatch(ctx, w, trimmed)
		return
	}

	var req Request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		s.writeJSON(w, s.errorResponseWithNull(InvalidRequest, "invalid request", err.Error()))
		return
	}

	if resp := s.handleRequest(ctx, &req); resp != nil {
		s.writeJSON(w, resp)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *httpServer) handleBatch(ctx context.Context, w http.ResponseWriter, raw json.RawMessage) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		s.writeJSON(w, s.errorResponseWithNull(InvalidRequest, "invalid request", err.Error()))
		return
	}
	if len(entries) == 0 {
		s.writeJSON(w, s.errorResponseWithNull(InvalidRequest, "invalid request", "empty batch"))
		return
	}

	responses := make([]*Response, 0, len(entries))

	for _, rawEntry := range entries {
		var req Request
		if err := json.Unmarshal(rawEntry, &req); err != nil {
			responses = append(responses, s.errorResponseWithNull(InvalidRequest, "invalid request", err.Error()))
			continue
		}
		if resp := s.handleRequest(ctx, &req); resp != nil {
			responses = append(responses, resp)
		}
	}

	if len(responses) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.writeJSON(w, responses)
}

func (s *httpServer) handleRequest(ctx context.Context, req *Request) *Response {
	if req.Method == "" {
		return s.errorResponse(req.ID, InvalidRequest, "method is required", nil)
	}
	if req.JSONRPC != Version {
		return s.errorResponse(req.ID, InvalidRequest, "invalid JSON-RPC version", nil)
	}

	resp, err := s.handler.Handle(ctx, req)

	if req.ID == nil {
		return nil
	}

	if err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			return s.errorResponse(req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		}
		return s.errorResponse(req.ID, InternalError, err.Error(), nil)
	}

	if resp == nil {
		resp = &Response{}
	}
	resp.JSONRPC = Version
	resp.ID = req.ID

	return resp
}

func (s *httpServer) writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *httpServer) writeError(w http.ResponseWriter, resp *Response, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if resp != nil {
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (s *httpServer) errorResponse(id any, code int, message string, data any) *Response {
	if id == nil {
		return nil
	}
	return &Response{
		JSONRPC: Version,
		Error: &Error{
			Code:    code,
			Message: message,
			Data:    data,
		},
		ID: id,
	}
}

func (s *httpServer) errorResponseWithNull(code int, message string, data any) *Response {
	return &Response{
		JSONRPC: Version,
		Error: &Error{
			Code:    code,
			Message: message,
			Data:    data,
		},
		ID: nil,
	}
}
