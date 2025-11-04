package jsonrpc2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
)

// RawClient implements a JSON-RPC 2.0 client over an io.ReadWriteCloser transport.
type RawClient struct {
	conn     io.ReadWriteCloser
	encoder  *json.Encoder
	decoder  *json.Decoder
	pending  map[string]chan callResult
	pendMu   sync.Mutex
	writeMu  sync.Mutex
	closed   chan struct{}
	closeErr error
	closeMu  sync.Mutex
}

var _ Caller = (*RawClient)(nil)

type callResult struct {
	resp *Response
	err  error
}

// NewRawClient constructs a Client that communicates over rwc.
func NewRawClient(rwc io.ReadWriteCloser) *RawClient {
	dec := json.NewDecoder(rwc)
	dec.UseNumber()
	c := &RawClient{
		conn:    rwc,
		encoder: json.NewEncoder(rwc),
		decoder: dec,
		pending: make(map[string]chan callResult),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Call sends the provided requests and returns their corresponding responses. The
// payload is encoded as a JSON array even when only a single request is supplied.
// Responses are aligned with the order of the supplied requests, and notifications
// (requests without an id) yield nil entries in the returned slice.
func (c *RawClient) Call(ctx context.Context, requests ...*Request) ([]*Response, error) {
	if len(requests) == 0 {
		return nil, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, c.CloseError()
	default:
	}

	type pendingEntry struct {
		id  string
		idx int
		ch  chan callResult
	}

	responses := make([]*Response, len(requests))
	pendings := make([]pendingEntry, 0, len(requests))

	cleanup := func() {
		for _, p := range pendings {
			c.removePending(p.id)
		}
	}

	for i, req := range requests {
		if req == nil {
			cleanup()
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
			cleanup()
			return nil, fmt.Errorf("jsonrpc2: invalid request id at index %d", i)
		}
		respCh := make(chan callResult, 1)
		c.pendMu.Lock()
		if _, exists := c.pending[idKey]; exists {
			c.pendMu.Unlock()
			cleanup()
			return nil, fmt.Errorf("jsonrpc2: duplicate request id %s", idKey)
		}
		c.pending[idKey] = respCh
		c.pendMu.Unlock()
		pendings = append(pendings, pendingEntry{id: idKey, idx: i, ch: respCh})
	}

	if err := c.sendRequests(requests); err != nil {
		cleanup()
		return nil, err
	}

	for _, pending := range pendings {
		select {
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case <-c.closed:
			cleanup()
			return nil, c.CloseError()
		case res := <-pending.ch:
			if res.err != nil {
				cleanup()
				return nil, res.err
			}
			responses[pending.idx] = res.resp
		}
	}

	return responses, nil
}

// Close terminates the underlying transport and releases resources.
func (c *RawClient) Close() error {
	c.closeMu.Lock()
	if c.closeErr != nil {
		err := c.closeErr
		c.closeMu.Unlock()
		return err
	}
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	if err := c.conn.Close(); err != nil {
		c.closeErr = err
	} else {
		c.closeErr = io.EOF
	}
	cerr := c.closeErr
	c.closeMu.Unlock()
	c.failPending(cerr)
	return cerr
}

// CloseError reports the error that caused the client to close, if any.
func (c *RawClient) CloseError() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closeErr
}

func (c *RawClient) readLoop() {
	for {
		var raw json.RawMessage
		if err := c.decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				c.failPending(io.EOF)
			} else {
				c.failPending(err)
			}
			return
		}

		raw = json.RawMessage(bytes.TrimSpace(raw))
		if len(raw) == 0 {
			continue
		}

		if raw[0] == '[' {
			var batch []Response
			if err := json.Unmarshal(raw, &batch); err != nil {
				c.failPending(err)
				return
			}
			for i := range batch {
				c.dispatchResponse(&batch[i])
			}
			continue
		}

		var resp Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			c.failPending(err)
			return
		}
		c.dispatchResponse(&resp)
	}
}

func (c *RawClient) sendRequests(requests []*Request) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.encoder.Encode(requests)
}

func (c *RawClient) dispatchResponse(resp *Response) {
	if resp.JSONRPC != Version {
		c.failPending(&Error{Code: InvalidRequest, Message: "invalid JSON-RPC version"})
		return
	}
	if resp.ID == nil {
		return
	}
	idKey, ok := normalizeID(resp.ID)
	if !ok {
		return
	}
	c.pendMu.Lock()
	ch, exists := c.pending[idKey]
	if exists {
		delete(c.pending, idKey)
	}
	c.pendMu.Unlock()
	if exists {
		respCopy := *resp
		ch <- callResult{resp: &respCopy}
	}
}

func (c *RawClient) removePending(id string) chan callResult {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	return ch
}

func (c *RawClient) failPending(err error) {
	if err == nil {
		err = io.EOF
	}

	c.closeMu.Lock()
	if c.closeErr == nil {
		c.closeErr = err
	} else {
		err = c.closeErr
	}
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	c.closeMu.Unlock()

	c.pendMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- callResult{err: err}
	}
	c.pendMu.Unlock()
}

func normalizeID(id any) (string, bool) {
	switch v := id.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case float64:
		if math.Trunc(v) != v {
			return "", false
		}
		return strconv.FormatInt(int64(v), 10), true
	case int:
		return strconv.Itoa(v), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	default:
		return "", false
	}
}
