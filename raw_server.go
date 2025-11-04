package jsonrpc2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Server processes JSON-RPC requests over an io.ReadWriteCloser transport.
type RawServer struct {
	conn     io.ReadWriteCloser
	encoder  *json.Encoder
	decoder  *json.Decoder
	handler  Handler
	writeMu  sync.Mutex
	closed   chan struct{}
	closeErr error
	closeMu  sync.Mutex
}

// NewRawServer constructs a Server that uses handler to process incoming requests.
func NewRawServer(rwc io.ReadWriteCloser, handler Handler) *RawServer {
	if handler == nil {
		panic("jsonrpc: handler cannot be nil")
	}
	dec := json.NewDecoder(rwc)
	dec.UseNumber()
	return &RawServer{
		conn:    rwc,
		encoder: json.NewEncoder(rwc),
		decoder: dec,
		handler: handler,
		closed:  make(chan struct{}),
	}
}

// Serve reads requests from the transport until the context is canceled or the
// connection terminates. It launches a goroutine per request so handler calls
// can run concurrently. The returned error is nil when the peer closes the
// connection cleanly.
func (s *RawServer) Serve(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			s.fail(ctx.Err())
			return ctx.Err()
		case <-s.closed:
			return s.CloseError()
		default:
		}

		var raw json.RawMessage
		if err := s.decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				s.fail(io.EOF)
				return nil
			}
			s.fail(err)
			return err
		}

		raw = json.RawMessage(bytes.TrimSpace(raw))
		if len(raw) == 0 {
			s.sendResponse(s.errorResponseWithNull(InvalidRequest, "invalid request", "empty payload"))
			continue
		}

		if raw[0] == '[' {
			s.handleBatch(ctx, raw)
			continue
		}

		var req Request
		if err := json.Unmarshal(raw, &req); err != nil {
			s.sendResponse(s.errorResponseWithNull(InvalidRequest, "invalid request", err.Error()))
			continue
		}

		if resp := s.handleRequest(ctx, &req); resp != nil {
			s.sendResponse(resp)
		}
	}
}

// Close shuts down the server and closes the underlying transport.
func (s *RawServer) Close() error {
	s.closeMu.Lock()
	if s.closeErr != nil {
		err := s.closeErr
		s.closeMu.Unlock()
		return err
	}
	if err := s.conn.Close(); err != nil {
		s.closeErr = err
	} else {
		s.closeErr = io.EOF
	}
	s.closeOnce()
	err := s.closeErr
	s.closeMu.Unlock()
	return err
}

// CloseError reports the error that closed the server, if any.
func (s *RawServer) CloseError() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closeErr
}

func (s *RawServer) handleRequest(ctx context.Context, req *Request) *Response {
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

func (s *RawServer) handleBatch(ctx context.Context, raw json.RawMessage) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		s.sendResponse(s.errorResponseWithNull(InvalidRequest, "invalid request", err.Error()))
		return
	}
	if len(entries) == 0 {
		s.sendResponse(s.errorResponseWithNull(InvalidRequest, "invalid request", "empty batch"))
		return
	}

	type batchResult struct {
		idx  int
		resp *Response
	}

	responses := make(map[int]*Response, len(entries))
	resultCh := make(chan batchResult, len(entries))
	var wg sync.WaitGroup

	for i, element := range entries {
		var req Request
		if err := json.Unmarshal(element, &req); err != nil {
			responses[i] = s.errorResponseWithNull(InvalidRequest, "invalid request", err.Error())
			continue
		}
		reqCopy := req
		wg.Add(1)
		go func(idx int, r Request) {
			defer wg.Done()
			if resp := s.handleRequest(ctx, &r); resp != nil {
				resultCh <- batchResult{idx: idx, resp: resp}
			}
		}(i, reqCopy)
	}

	wg.Wait()
	close(resultCh)

	for res := range resultCh {
		responses[res.idx] = res.resp
	}

	ordered := make([]*Response, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		if resp, ok := responses[i]; ok && resp != nil {
			ordered = append(ordered, resp)
		}
	}

	s.sendBatch(ordered)
}

func (s *RawServer) sendResponse(resp *Response) {
	if resp == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.encoder.Encode(resp); err != nil {
		s.fail(err)
	}
}

func (s *RawServer) sendBatch(resps []*Response) {
	if len(resps) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.encoder.Encode(resps); err != nil {
		s.fail(err)
	}
}

func (s *RawServer) errorResponse(id any, code int, message string, data any) *Response {
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

func (s *RawServer) errorResponseWithNull(code int, message string, data any) *Response {
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

func (s *RawServer) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	s.closeMu.Lock()
	if s.closeErr == nil {
		s.closeErr = err
		_ = s.conn.Close()
	}
	s.closeOnce()
	s.closeMu.Unlock()
}

func (s *RawServer) closeOnce() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
}
