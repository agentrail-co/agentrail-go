package agentrail_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func attr(attrs []*common.KeyValue, k string) *common.AnyValue {
	for _, a := range attrs {
		if a.Key == k {
			return a.Value
		}
	}
	return nil
}
func spans(r *receiver) []*tracepb.Span {
	out := []*tracepb.Span{}
	for _, v := range r.traces {
		for _, rs := range v.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}
func TestIdentityIsolationAndConflicts(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		id := q.Header.Get("X-Synthetic-Identity")
		if err := c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: id}); err != nil {
			t.Error(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: id})
				c.Milestone(q.Context(), "work", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "work"})
			}()
		}
		wg.Wait()
		c.Identify(q.Context(), agentrail.VerifiedIdentity{Kind: agentrail.PrincipalUser, ID: id + "-conflict"})
	}))
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			q := httptest.NewRequest("GET", "http://app.test/", nil)
			q.Header.Set("X-Synthetic-Identity", id)
			h.ServeHTTP(httptest.NewRecorder(), q)
		}(id)
	}
	wg.Wait()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]string{}
	conflicts := 0
	for _, sp := range spans(r) {
		v := attr(sp.Attributes, "agentrail.principal.id_hash")
		if v == nil {
			t.Fatal("missing identity")
		}
		key := string(sp.TraceId)
		if sp.Name == "identity.asserted" {
			conflicts++
			continue
		}
		if before, ok := seen[key]; ok && before != v.GetStringValue() {
			t.Fatal("identity drift")
		}
		seen[key] = v.GetStringValue()
	}
	if len(seen) != 2 || conflicts != 2 {
		t.Fatalf("wrong roots/conflicts %d/%d", len(seen), conflicts)
	}
	var ids []string
	for _, id := range seen {
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatal("cross-request identity leak")
	}
}
func TestMilestonePreservesErrorsPanicsAndChildScope(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	sentinel := errors.New("private-failure")
	if got := c.WithMilestone(context.Background(), "operation", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "operation"}, func(ctx context.Context) error { return sentinel }); got != sentinel {
		t.Fatal("replaced error")
	}
	func() {
		defer func() {
			if got := recover(); got != sentinel {
				t.Fatal("replaced panic")
			}
		}()
		c.WithMilestone(context.Background(), "panic", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "operation"}, func(context.Context) error { panic(sentinel) })
	}()
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(spans(r)) != 2 {
		t.Fatal("missing steps")
	}
	for _, sp := range spans(r) {
		if sp.Status.Code != 2 {
			t.Fatal("missing failure status")
		}
		if strings.Contains(fmt.Sprint(sp), "private-failure") {
			t.Fatal("error message exported")
		}
	}
}
