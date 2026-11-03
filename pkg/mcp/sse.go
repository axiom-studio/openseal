package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// sseStream is a legacy (2024-11-05) SSE connection: requests are POSTed to
// the endpoint the stream announces, and responses arrive on the stream,
// routed to their waiting request by ID.
type sseStream struct {
	body     io.ReadCloser
	cancel   context.CancelFunc
	endpoint string
	mu       sync.Mutex
	pending  map[int64]chan reply
	failure  error
}

type reply struct {
	result map[string]interface{}
	err    error
}

func (s *Session) startSSE(ctx context.Context) error {
	// The stream lives as long as the Session, not the call that opened it.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	resp, err := s.do(streamCtx, http.MethodGet, s.client.config.Endpoint, nil, http.Header{"Accept": {"text/event-stream"}})
	if err != nil {
		cancel()
		return err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		cancel()
		return errors.New("legacy MCP endpoint did not return SSE")
	}
	stream := &sseStream{body: resp.Body, cancel: cancel, pending: map[int64]chan reply{}}
	endpoints := make(chan string, 1)
	go stream.read(endpoints)
	select {
	case <-ctx.Done():
		stream.close()
		return errors.New("MCP SSE endpoint discovery timed out")
	case endpoint, ok := <-endpoints:
		if !ok {
			stream.close()
			return errors.New("MCP SSE stream has no message endpoint")
		}
		base, _ := url.Parse(s.client.config.Endpoint)
		relative, err := url.Parse(endpoint)
		if err != nil {
			stream.close()
			return errors.New("invalid MCP SSE message endpoint")
		}
		resolved := base.ResolveReference(relative)
		if resolved.Scheme != base.Scheme || resolved.Host != base.Host || resolved.User != nil || resolved.Fragment != "" {
			stream.close()
			return errors.New("MCP SSE message endpoint must stay on the configured origin")
		}
		stream.endpoint = resolved.String()
	}
	s.sse = stream
	return nil
}

func (st *sseStream) read(endpoints chan<- string) {
	defer close(endpoints)
	defer st.fail(errors.New("MCP SSE stream ended; outcome may be unknown"))
	scan := bufio.NewScanner(st.body)
	scan.Buffer(make([]byte, 4096), MaxBytes)
	event, data, size, announced := "", []string{}, 0, false
	for scan.Scan() {
		line := scan.Text()
		if size += len(line) + 1; size > MaxBytes {
			st.fail(errors.New("MCP SSE event exceeds 2 MiB"))
			return
		}
		switch {
		case line == "":
			if len(data) > 0 {
				value := strings.Join(data, "\n")
				if event == "endpoint" && !announced {
					announced = true
					endpoints <- value
				} else if event == "message" || event == "" {
					st.dispatch([]byte(value))
				}
			}
			event, data, size = "", nil, 0
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

func (st *sseStream) dispatch(raw []byte) {
	var msg envelope
	if err := json.Unmarshal(raw, &msg); err != nil || msg.JSONRPC != "2.0" {
		st.fail(errors.New("invalid MCP JSON-RPC envelope"))
		return
	}
	if msg.Method != "" {
		if msg.ID != nil {
			st.fail(errors.New("server-initiated MCP requests are not supported"))
		}
		return
	}
	if msg.ID == nil {
		st.fail(errors.New("MCP response ID missing"))
		return
	}
	st.mu.Lock()
	ch := st.pending[*msg.ID]
	delete(st.pending, *msg.ID)
	st.mu.Unlock()
	if ch != nil {
		result, _, err := msg.answer(*msg.ID)
		ch <- reply{result: result, err: err}
	}
}

// fail ends the stream for every waiting request; the first failure wins.
func (st *sseStream) fail(err error) {
	st.mu.Lock()
	if st.failure == nil {
		st.failure = err
	}
	for id, ch := range st.pending {
		ch <- reply{err: st.failure}
		delete(st.pending, id)
	}
	st.mu.Unlock()
	st.cancel()
}

func (st *sseStream) close() {
	st.fail(errors.New("MCP session closed"))
	st.body.Close()
}

// send POSTs one message and, for a request (id != 0), waits for its
// response on the stream.
func (st *sseStream) send(ctx context.Context, s *Session, id int64, data []byte) (map[string]interface{}, error) {
	var ch chan reply
	if id != 0 {
		st.mu.Lock()
		if st.failure != nil {
			err := st.failure
			st.mu.Unlock()
			return nil, err
		}
		ch = make(chan reply, 1)
		st.pending[id] = ch
		st.mu.Unlock()
	}
	forget := func() {
		st.mu.Lock()
		delete(st.pending, id)
		st.mu.Unlock()
	}
	resp, err := s.do(ctx, http.MethodPost, st.endpoint, data, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		forget()
		return nil, err
	}
	resp.Body.Close()
	if id == 0 {
		return nil, nil
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		forget()
		return nil, errors.New("MCP response deadline exceeded; outcome may be unknown")
	}
}
