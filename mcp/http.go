package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sync"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	"github.com/felixge/httpsnoop"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

const (
	// messageLimit is the largest request body or response message read for
	// capture; a larger one passes through unread and unrecorded.
	messageLimit = 64 << 20
	// sessionLimit bounds how many Streamable HTTP sessions' client metadata
	// is remembered; the oldest is forgotten first.
	sessionLimit   = 4096
	sessionHeader  = "Mcp-Session-Id"
	protocolHeader = "Mcp-Protocol-Version"
)

// Handler wraps an sdk.StreamableHTTPHandler or sdk.SSEHandler so every
// JSON-RPC message the server receives becomes a transport span, including
// requests the SDK refuses before any middleware runs. Mount it inside
// agentrail's Client.Middleware so each span joins its HTTP request's journey.
// Response streaming and the writer's optional interfaces are preserved.
func Handler(client *agentrail.Client, next http.Handler) http.Handler {
	return &httpCapture{client: client, next: next, sessions: map[string]mcpspan.Meta{}, sse: map[string]*tracker{}}
}

type httpCapture struct {
	client *agentrail.Client
	next   http.Handler
	mu     sync.Mutex
	// sessions holds what a Streamable HTTP session's initialize said.
	sessions map[string]mcpspan.Meta
	order    []string
	// sse maps a legacy SSE session to the tracker of its stream.
	sse map[string]*tracker
}

func (h *httpCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodGet:
		h.stream(w, r)
	case http.MethodDelete:
		h.next.ServeHTTP(w, r)
		h.forget(r.Header.Get(sessionHeader))
	default:
		h.next.ServeHTTP(w, r)
	}
}

func (h *httpCapture) remember(id string, m mcpspan.Meta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.sessions[id]; !ok {
		h.order = append(h.order, id)
		if len(h.order) > sessionLimit {
			delete(h.sessions, h.order[0])
			h.order = h.order[1:]
		}
	}
	m.SessionID = ""
	h.sessions[id] = m
}
func (h *httpCapture) forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
}

// teeBody copies a request body as the handler reads it and, when the handler
// has read it all, passes the copy to done before returning io.EOF. The MCP
// SDK reads a POST body whole before acting on it, so done runs before any
// message is handled; a body the SDK refuses unread, or stops reading at its
// size limit, never reaches done. A body over messageLimit is not kept.
type teeBody struct {
	io.ReadCloser
	data  []byte
	over  bool
	fired bool
	done  func([]byte)
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.over {
		if len(b.data)+n > messageLimit {
			b.over, b.data = true, nil
		} else {
			b.data = append(b.data, p[:n]...)
		}
	}
	if err == io.EOF && !b.fired {
		b.fired = true
		if !b.over {
			b.done(b.data)
		}
		b.data = nil
	}
	return n, err
}

// decode reads one JSON-RPC message or a batch of them.
func decode(data []byte) []jsonrpc.Message {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var raw []json.RawMessage
		if json.Unmarshal(data, &raw) != nil {
			return nil
		}
		out := []jsonrpc.Message{}
		for _, r := range raw {
			if m, err := jsonrpc.DecodeMessage(r); err == nil {
				out = append(out, m)
			}
		}
		return out
	}
	if m, err := jsonrpc.DecodeMessage(data); err == nil {
		return []jsonrpc.Message{m}
	}
	return nil
}

func (h *httpCapture) post(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody {
		h.next.ServeHTTP(w, r)
		return
	}
	r = r.WithContext(r.Context())
	if id := r.URL.Query().Get("sessionid"); id != "" {
		h.mu.Lock()
		t := h.sse[id]
		h.mu.Unlock()
		if t != nil {
			h.ssePost(w, r, t)
			return
		}
	}
	// Streamable HTTP: answers come back on this request.
	requested := r.Header.Get(sessionHeader)
	h.mu.Lock()
	meta, issued := h.sessions[requested]
	h.mu.Unlock()
	// The version this message carried comes before what initialize said.
	if v := r.Header.Get(protocolHeader); v != "" {
		meta.Protocol = v
	}
	t := newTracker(h.client, meta)
	// Only an ID the server issued on an initialize answer is a session; a
	// stateless server issues none, whatever the request carries.
	if issued {
		t.setSession(requested)
	}
	key := headerKeyOf(r.Header)
	var mu sync.Mutex // the SDK may write from its session goroutine
	initialize := false
	received := r.Context()
	r.Body = &teeBody{ReadCloser: r.Body, done: func(data []byte) {
		for _, m := range decode(data) {
			if req, ok := m.(*jsonrpc.Request); ok && req.Method == "initialize" {
				mu.Lock()
				initialize = true
				mu.Unlock()
			}
			t.receive(received, m, key)
		}
	}}
	// A stateful session's handlers run in its initialize request's context,
	// so this marker outlives the request: it is a fresh token no call is
	// registered under, never the header key whose address may be reused.
	ctx := context.WithValue(r.Context(), activeKey{}, new(postMarker))
	status := 0
	// Deferred so a panicking handler leaves nothing registered.
	defer func() {
		mu.Lock()
		rejected := status >= 400
		mu.Unlock()
		t.abandon(rejected)
	}()
	var body bytes.Buffer
	sse := &sseParser{onEvent: func(_ string, data []byte) {
		for _, m := range decode(data) {
			t.respond(m)
		}
	}}
	streaming := false
	header := func(code int) {
		mu.Lock()
		defer mu.Unlock()
		if status != 0 {
			return
		}
		status = code
		issued := w.Header().Get(sessionHeader)
		switch {
		case code == http.StatusNotFound:
			t.setSession("") // not a session the server knows
		case issued != "":
			t.setSession(issued)
		}
		media, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type"))
		streaming = media == "text/event-stream"
	}
	write := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		if streaming {
			sse.write(b)
			return
		}
		if body.Len()+len(b) > messageLimit {
			body.Reset()
			return
		}
		body.Write(b)
		if json.Valid(body.Bytes()) { // a whole JSON answer, usually one write
			for _, m := range decode(body.Bytes()) {
				t.respond(m)
			}
			body.Reset()
		}
	}
	h.next.ServeHTTP(capture(w, header, write), r.WithContext(ctx))
	mu.Lock()
	defer mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if !streaming {
		for _, m := range decode(body.Bytes()) {
			t.respond(m)
		}
	}
	if initialize && status < 300 {
		if issued := w.Header().Get(sessionHeader); issued != "" {
			h.remember(issued, t.snapshot())
		}
	}
}

// ssePost records a legacy SSE message; its answer arrives on the stream.
func (h *httpCapture) ssePost(w http.ResponseWriter, r *http.Request, t *tracker) {
	var mu sync.Mutex
	added := []*call{}
	received := r.Context()
	r.Body = &teeBody{ReadCloser: r.Body, done: func(data []byte) {
		for _, m := range decode(data) {
			if c := t.receive(received, m, t); c != nil {
				mu.Lock()
				added = append(added, c)
				mu.Unlock()
			}
		}
	}}
	status := 0
	returned := false
	// Deferred so a panicking handler leaves nothing registered. Only this
	// POST's calls end here: the stream's tracker holds other POSTs' too, and
	// abandon with none given would end them all.
	defer func() {
		mu.Lock()
		ended, rejected := added, status >= 400
		mu.Unlock()
		if len(ended) > 0 && (rejected || !returned) {
			t.abandon(rejected, ended...)
		}
	}()
	h.next.ServeHTTP(capture(w, func(code int) {
		mu.Lock()
		defer mu.Unlock()
		if status == 0 {
			status = code
		}
	}, func([]byte) {}), r)
	returned = true
}

// stream follows a GET stream: a legacy SSE session, or a Streamable HTTP
// session's stream of server messages.
func (h *httpCapture) stream(w http.ResponseWriter, r *http.Request) {
	t := newTracker(h.client, mcpspan.Meta{})
	sessionID := "" // guarded by the parser's lock
	sse := &sseParser{}
	sse.onEvent = func(event string, data []byte) {
		if event == "endpoint" {
			if u, err := url.Parse(string(data)); err == nil && sessionID == "" {
				sessionID = u.Query().Get("sessionid")
				if sessionID != "" {
					h.mu.Lock()
					h.sse[sessionID] = t
					h.mu.Unlock()
				}
			}
			return
		}
		for _, m := range decode(data) {
			t.respond(m)
		}
	}
	defer func() {
		sse.mu.Lock()
		defer sse.mu.Unlock()
		if sessionID != "" {
			h.mu.Lock()
			delete(h.sse, sessionID)
			h.mu.Unlock()
		}
		// Closed, not only abandoned: a POST that found this stream's tracker
		// just before it ended must not register a call nobody cleans up.
		t.close()
	}()
	// An SSE session's handlers run in this request's context.
	ctx := context.WithValue(r.Context(), activeKey{}, any(t))
	h.next.ServeHTTP(capture(w, func(int) {}, sse.write), r.WithContext(ctx))
}

// capture observes the status and body bytes the handler writes, without
// changing them or the writer's optional interfaces.
func capture(w http.ResponseWriter, header func(int), write func([]byte)) http.ResponseWriter {
	var inner httpsnoop.WriteFunc
	// Bytes are observed before they reach the client, so a response's span
	// has ended by the time the client can read it.
	hooked := func(b []byte) (int, error) {
		header(http.StatusOK)
		write(b)
		return inner(b)
	}
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) {
				if code >= 200 {
					header(code)
				}
				next(code)
			}
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			inner = next
			return hooked
		},
		ReadFrom: func(httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(src io.Reader) (int64, error) { return io.Copy(writerFunc(hooked), src) }
		},
	})
}

type postMarker struct{ _ byte }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// sseParser reads server-sent events as the SDK writes them: field lines,
// with data lines joined by newlines, ended by a blank line.
type sseParser struct {
	mu       sync.Mutex
	line     []byte
	event    string
	data     []byte
	hasData  bool
	overflow bool
	onEvent  func(event string, data []byte)
}

func (p *sseParser) write(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.append(b)
			return
		}
		p.append(b[:i])
		p.field(bytes.TrimSuffix(p.line, []byte("\r")))
		p.line = p.line[:0]
		b = b[i+1:]
	}
}
func (p *sseParser) append(b []byte) {
	if len(p.line)+len(p.data)+len(b) > messageLimit {
		p.overflow = true
		return
	}
	p.line = append(p.line, b...)
}
func (p *sseParser) field(line []byte) {
	if len(line) == 0 {
		if p.hasData && !p.overflow {
			p.onEvent(p.event, p.data)
		}
		p.event, p.data, p.hasData, p.overflow = "", nil, false, false
		return
	}
	name, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(name) {
	case "event":
		p.event = string(value)
	case "data":
		if p.hasData {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, value...)
		p.hasData = true
	}
}
