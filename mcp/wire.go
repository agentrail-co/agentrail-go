package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"

	agentrail "github.com/agentrail-co/agentrail-go"
	"github.com/agentrail-co/agentrail-go/internal/mcpspan"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A call is a received JSON-RPC request waiting for its response.
type call struct {
	span           mcpspan.Span
	method, target string
	meta           mcpspan.Meta
	keys           []any
	claimed        bool // guarded by registry.mu
}

// The registry lets a handler wrapper find the transport span of the request
// it serves. Keys are exact: the *sdk.RequestExtra a Connection wrapper gave
// the request, the identity of the HTTP request's header map (the MCP SDK
// hands that same map to the handler), or the tracker a legacy SSE stream
// put in the session's context.
var registry = struct {
	mu    sync.Mutex
	calls map[any][]*call
}{calls: map[any][]*call{}}

type headerKey struct{ p uintptr }

func headerKeyOf(h http.Header) any { return headerKey{reflect.ValueOf(h).Pointer()} }

func register(c *call) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, k := range c.keys {
		registry.calls[k] = append(registry.calls[k], c)
	}
}
func unregister(c *call) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, k := range c.keys {
		list := registry.calls[k]
		for i, other := range list {
			if other == c {
				list = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(registry.calls, k)
		} else {
			registry.calls[k] = list
		}
	}
}

// activeKey marks a context whose session a transport wrapper observes.
type activeKey struct{}

// lookup finds and claims the transport call a handler serves. active is true
// when a transport wrapper observes the request even if no call matched, so
// the handler never becomes a second step for one call.
func lookup(ctx context.Context, extra *sdk.RequestExtra, method, target string) (mcpspan.Span, bool) {
	keys := []any{}
	if extra != nil {
		keys = append(keys, extra)
		if extra.Header != nil {
			keys = append(keys, headerKeyOf(extra.Header))
		}
	}
	marker := ctx.Value(activeKey{})
	if marker != nil {
		keys = append(keys, marker)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	active := marker != nil
	for _, k := range keys {
		list, ok := registry.calls[k]
		active = active || ok
		for _, c := range list {
			if !c.claimed && c.method == method && c.target == target {
				c.claimed = true
				return c.span, true
			}
		}
	}
	return nil, active
}

// tracker follows the messages of one connection, HTTP request or stream.
type tracker struct {
	client  *agentrail.Client
	mu      sync.Mutex
	meta    mcpspan.Meta
	pending map[string]*call
	closed  bool // set by close; no call is kept pending after it
}

func newTracker(client *agentrail.Client, meta mcpspan.Meta) *tracker {
	return &tracker{client: client, meta: meta, pending: map[string]*call{}}
}
func idKey(id jsonrpc.ID) string { return fmt.Sprintf("%T:%v", id.Raw(), id.Raw()) }

type paramsView struct {
	Name            string `json:"name"`
	URI             string `json:"uri"`
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
	Meta struct {
		Protocol   string `json:"io.modelcontextprotocol/protocolVersion"`
		ClientInfo *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"io.modelcontextprotocol/clientInfo"`
	} `json:"_meta"`
}

// receive starts the transport span of a request or notification. A request
// waits for respond; keys let its handler find it. It returns the call.
func (t *tracker) receive(ctx context.Context, msg jsonrpc.Message, keys ...any) *call {
	req, ok := msg.(*jsonrpc.Request)
	if !ok {
		return nil // a client's answer to a server request is not a step
	}
	size := -1
	if data, err := jsonrpc.EncodeMessage(req); err == nil {
		size = len(data)
	}
	var p paramsView
	if len(req.Params) > 0 {
		// Params that are not an object carry no name; the SDK answers them.
		_ = json.Unmarshal(req.Params, &p)
	}
	target := ""
	switch req.Method {
	case "tools/call", "prompts/get":
		target = p.Name
	case "resources/read":
		target = p.URI
	}
	t.mu.Lock()
	if req.Method == "initialize" {
		t.meta.Protocol = p.ProtocolVersion
		if p.ClientInfo != nil {
			t.meta.Client, t.meta.ClientVersion = p.ClientInfo.Name, p.ClientInfo.Version
		}
	}
	meta := t.meta
	t.mu.Unlock()
	// Requests of the sessionless protocol describe their client themselves.
	if p.Meta.Protocol != "" {
		meta.Protocol = p.Meta.Protocol
	}
	if p.Meta.ClientInfo != nil {
		meta.Client, meta.ClientVersion = p.Meta.ClientInfo.Name, p.Meta.ClientInfo.Version
	}
	span := mcpspan.Transport(t.client, ctx, mcpspan.Call{Method: req.Method, Target: target, RequestBytes: size, Params: req.Params})
	c := &call{span: span, method: req.Method, target: target, meta: meta, keys: keys}
	if !req.IsCall() {
		span.End(mcpspan.Outcome{Meta: t.sessionMeta(meta)})
		return nil
	}
	// Registered before it is pending, so whoever takes it from pending (an
	// answer, which with a repeated id can be the earlier call's) unregisters it.
	register(c)
	t.mu.Lock()
	if t.closed { // read while its connection closed: no answer will come
		t.mu.Unlock()
		unregister(c)
		span.End(mcpspan.Outcome{Meta: t.sessionMeta(meta)})
		return nil
	}
	// A repeated id replaces a call still pending, which can then never be
	// matched to its answer: end it as refused so nothing stays registered.
	replaced := t.pending[idKey(req.ID)]
	t.pending[idKey(req.ID)] = c
	t.mu.Unlock()
	if replaced != nil {
		unregister(replaced)
		replaced.span.End(mcpspan.Outcome{Meta: t.sessionMeta(replaced.meta), Rejected: true})
	}
	return c
}

func (t *tracker) sessionMeta(m mcpspan.Meta) mcpspan.Meta {
	t.mu.Lock()
	defer t.mu.Unlock()
	m.SessionID = t.meta.SessionID
	return m
}
func (t *tracker) snapshot() mcpspan.Meta {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.meta
}
func (t *tracker) setSession(id string) {
	t.mu.Lock()
	t.meta.SessionID = id
	t.mu.Unlock()
}

type responseView struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// respond ends the transport span of the request a response answers.
func (t *tracker) respond(msg jsonrpc.Message) {
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		return
	}
	t.mu.Lock()
	c := t.pending[idKey(resp.ID)]
	delete(t.pending, idKey(resp.ID))
	t.mu.Unlock()
	if c == nil {
		return
	}
	unregister(c)
	o := mcpspan.Outcome{Meta: t.sessionMeta(c.meta), Responded: true}
	// Everything is read from the bytes the SDK writes, so the error code is
	// the one the client receives, whatever Go error produced it.
	if data, err := jsonrpc.EncodeMessage(resp); err == nil {
		o.ResponseBytes = len(data)
		var v responseView
		if json.Unmarshal(data, &v) == nil {
			if len(v.Error) > 0 && !bytes.Equal(v.Error, []byte("null")) {
				var e struct {
					Code int64 `json:"code"`
				}
				if json.Unmarshal(v.Error, &e) == nil {
					o.ErrorCode = &e.Code
				}
				o.Result = v.Error
			} else {
				o.Result = v.Result
				if c.method == "tools/call" {
					var r struct {
						IsError bool `json:"isError"`
					}
					o.IsError = json.Unmarshal(v.Result, &r) == nil && r.IsError
				}
			}
		}
	}
	c.span.End(o)
}

// close ends every pending call and keeps none received afterwards.
func (t *tracker) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.abandon(false)
}

// abandon ends calls that will get no response here: those given, or every
// pending call when none are. rejected marks a request the transport refused.
func (t *tracker) abandon(rejected bool, only ...*call) {
	t.mu.Lock()
	var ended []*call
	for id, c := range t.pending {
		if len(only) == 0 || contains(only, c) {
			ended = append(ended, c)
			delete(t.pending, id)
		}
	}
	t.mu.Unlock()
	for _, c := range ended {
		unregister(c)
		c.span.End(mcpspan.Outcome{Meta: t.sessionMeta(c.meta), Rejected: rejected})
	}
}
func contains(list []*call, c *call) bool {
	for _, other := range list {
		if other == c {
			return true
		}
	}
	return false
}
