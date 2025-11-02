package jsonrpc2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// HTTPClientOption configures the behaviour of an HTTPClient.
type HTTPClientOption func(*HTTPClient)

// WithHTTPClient overrides the underlying http.Client used to make requests.
func WithHTTPClient(client *http.Client) HTTPClientOption {
	return func(c *HTTPClient) {
		c.httpClient = client
	}
}

// WithHTTPHeader adds the provided header key/value pair to every request.
func WithHTTPHeader(key, value string) HTTPClientOption {
	return func(c *HTTPClient) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		c.headers.Add(key, value)
	}
}

// WithHTTPHeaders merges the supplied headers into the client's default headers.
func WithHTTPHeaders(headers http.Header) HTTPClientOption {
	return func(c *HTTPClient) {
		if c.headers == nil {
			c.headers = make(http.Header)
		}
		for k, values := range headers {
			for _, v := range values {
				c.headers.Add(k, v)
			}
		}
	}
}

// HTTPClient issues JSON-RPC requests over HTTP.
type HTTPClient struct {
	endpoint   string
	httpClient *http.Client
	headers    http.Header
	nextID     uint64
}

// NewHTTPClient constructs a JSON-RPC client that communicates with the given endpoint over HTTP.
func NewHTTPClient(endpoint string, opts ...HTTPClientOption) *HTTPClient {
	if endpoint == "" {
		panic("jsonrpc: endpoint cannot be empty")
	}

	c := &HTTPClient{
		endpoint:   endpoint,
		httpClient: http.DefaultClient,
		headers:    make(http.Header),
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}

	return c
}

// Call sends a single JSON-RPC request over HTTP and waits for the response.
func (c *HTTPClient) Call(ctx context.Context, method string, params any) (*Response, error) {
	id := atomic.AddUint64(&c.nextID, 1)
	idStr := fmt.Sprint(id)

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

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	body, status, err := c.do(ctx, payload)
	if err != nil {
		return nil, err
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("jsonrpc: unexpected HTTP status %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("jsonrpc: empty response body")
	}

	var resp Response
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// Notify sends a JSON-RPC notification (no response expected) over HTTP.
func (c *HTTPClient) Notify(ctx context.Context, method string, params any) error {
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

	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}

	body, status, err := c.do(ctx, payload)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("jsonrpc: unexpected HTTP status %d", status)
	}

	if len(bytes.TrimSpace(body)) != 0 {
		return errors.New("jsonrpc: unexpected body in notification response")
	}

	return nil
}

// Batch sends a JSON-RPC batch request over HTTP and returns the responses in
// the same order as the supplied calls (notifications yield nil entries).
func (c *HTTPClient) Batch(ctx context.Context, calls []BatchCall) ([]*Response, error) {
	if len(calls) == 0 {
		return nil, nil
	}

	requests := make([]*Request, 0, len(calls))
	responses := make([]*Response, len(calls))
	idIndex := make(map[string]int)

	for i, call := range calls {
		req := &Request{
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
			idStr := fmt.Sprint(id)
			req.ID = idStr
			idIndex[idStr] = i
		}
		requests = append(requests, req)
	}

	payload, err := json.Marshal(requests)
	if err != nil {
		return nil, err
	}

	body, status, err := c.do(ctx, payload)
	if err != nil {
		return nil, err
	}

	if len(idIndex) == 0 {
		if status != http.StatusNoContent && status != http.StatusOK {
			return nil, fmt.Errorf("jsonrpc: unexpected HTTP status %d", status)
		}
		if len(bytes.TrimSpace(body)) != 0 {
			return nil, errors.New("jsonrpc: unexpected body for notification batch")
		}
		return responses, nil
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("jsonrpc: unexpected HTTP status %d", status)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("jsonrpc: empty response body")
	}

	var batch []Response
	if err := json.Unmarshal(trimmed, &batch); err != nil {
		return nil, err
	}

	for _, resp := range batch {
		if resp.JSONRPC != Version {
			return nil, errors.New("jsonrpc: invalid response version")
		}
		if resp.ID == nil {
			continue
		}
		idKey, ok := normalizeID(resp.ID)
		if !ok {
			continue
		}
		idx, exists := idIndex[idKey]
		if !exists {
			continue
		}
		respCopy := resp
		responses[idx] = &respCopy
		delete(idIndex, idKey)
	}

	if len(idIndex) != 0 {
		return nil, errors.New("jsonrpc: missing responses for batch requests")
	}

	return responses, nil
}

func (c *HTTPClient) do(ctx context.Context, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	for k, values := range c.headers {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.httpClient.Do(req)
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

// NewHTTPHandler adapts a JSON-RPC handler to net/http.
func NewHTTPHandler(handler Handler) http.Handler {
	if handler == nil {
		panic("jsonrpc: handler cannot be nil")
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

	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "application/json") {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	conn := newBufferConn(body)
	srv := NewServer(conn, s.handler)

	serveErr := srv.Serve(r.Context())
	closeErr := srv.CloseError()

	// Prefer the more specific error from Serve if available.
	if serveErr == nil {
		serveErr = closeErr
	}

	respBytes := conn.Bytes()

	if len(respBytes) == 0 {
		switch {
		case isParseError(serveErr):
			writeJSONError(w, nil, ParseError, "parse error", serveErr.Error())
		case serveErr != nil && !errors.Is(serveErr, io.EOF) && !errors.Is(serveErr, context.Canceled):
			http.Error(w, "internal server error", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

type bufferConn struct {
	reader  *bytes.Reader
	buf     bytes.Buffer
	bufMu   sync.Mutex
	closed  bool
	closeMu sync.Mutex
}

func newBufferConn(payload []byte) *bufferConn {
	return &bufferConn{reader: bytes.NewReader(payload)}
}

func (c *bufferConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		return 0, io.EOF
	}
	return c.reader.Read(p)
}

func (c *bufferConn) Write(p []byte) (int, error) {
	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	return c.buf.Write(p)
}

func (c *bufferConn) Close() error {
	c.closeMu.Lock()
	c.closed = true
	c.closeMu.Unlock()
	return nil
}

func (c *bufferConn) Bytes() []byte {
	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	if c.buf.Len() == 0 {
		return nil
	}
	cp := make([]byte, c.buf.Len())
	copy(cp, c.buf.Bytes())
	return cp
}

func isParseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	var invalidUnmarshal *json.InvalidUnmarshalError
	if errors.As(err, &invalidUnmarshal) {
		return true
	}
	return false
}

func writeJSONError(w http.ResponseWriter, id any, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	resp := Response{
		JSONRPC: Version,
		Error: &Error{
			Code:    code,
			Message: message,
			Data:    data,
		},
		ID: id,
	}
	enc := json.NewEncoder(w)
	_ = enc.Encode(&resp)
}
