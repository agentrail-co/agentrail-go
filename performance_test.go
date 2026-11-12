package agentrail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const namespaceID = "11111111-1111-4111-8111-111111111111"
const bindingID = "22222222-2222-4222-8222-222222222222"

// The Node SDK's own test vector: same key, namespace and reference, same hash.
func nodeOperationVector(t *testing.T) (instanceRef, hash string) {
	t.Helper()
	b, err := os.ReadFile("testdata/interoperability/operations.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Name, HashKey, NamespaceID, InstanceRef, Hash string }
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		if v.Name == "node-test-vector" {
			if v.HashKey != options("").HashKey || v.NamespaceID != namespaceID {
				t.Fatal("vector no longer matches the test options")
			}
			return v.InstanceRef, v.Hash
		}
	}
	t.Fatal("node-test-vector missing")
	return "", ""
}

func spanNamed(t *testing.T, all []*tracepb.Span, name string) *tracepb.Span {
	t.Helper()
	for _, sp := range all {
		if sp.Name == name {
			return sp
		}
	}
	t.Fatalf("span %q not exported", name)
	return nil
}

func stringAttr(sp *tracepb.Span, k string) string {
	if v := attr(sp.Attributes, k); v != nil {
		return v.GetStringValue()
	}
	return ""
}

// Fails if operation references leave the process unhashed, the hash diverges
// from agentrail-node's, evidence leaks onto child spans, or a plugin-only
// invocation is exported as a workflow outcome.
func TestPerformanceEvidenceIsHashedAndScopedToItsMilestone(t *testing.T) {
	r, s := collector(t)
	c, err := agentrail.New(context.Background(), options(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())
	instanceRef, hash := nodeOperationVector(t)
	start := agentrail.PerformanceContext{
		Environment: agentrail.EnvironmentProduction,
		Operation:   &agentrail.AuthorizedOperation{NamespaceID: namespaceID, InstanceRef: instanceRef, Role: agentrail.RoleStart},
		EventName:   "delivery.requested",
		Plugin:      &agentrail.PluginInvocation{BindingID: bindingID, InvocationID: "invocation-1"},
	}
	err = c.WithMilestone(context.Background(), "delivery.requested", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "business.event", Performance: &start}, func(ctx context.Context) error {
		c.Milestone(ctx, "child.step", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "child.step"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal := start
	terminal.Operation = &agentrail.AuthorizedOperation{NamespaceID: namespaceID, InstanceRef: instanceRef, Role: agentrail.RoleTerminal}
	terminal.EventName = "delivery.completed"
	c.Milestone(context.Background(), "delivery.completed", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "business.event", Performance: &terminal})
	next := start
	next.Operation = &agentrail.AuthorizedOperation{NamespaceID: namespaceID, InstanceRef: instanceRef + "-next", Role: agentrail.RoleStart}
	c.Milestone(context.Background(), "next.attempt", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "business.event", Performance: &next})
	other := start
	other.Operation = &agentrail.AuthorizedOperation{NamespaceID: bindingID, InstanceRef: instanceRef, Role: agentrail.RoleStart}
	c.Milestone(context.Background(), "other.namespace", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "business.event", Performance: &other})
	c.Milestone(context.Background(), "plugin.invoked", agentrail.MilestoneContext{Surface: agentrail.SurfaceMCP, Kind: "mcp.tool", Performance: &agentrail.PerformanceContext{
		Environment: agentrail.EnvironmentTest,
		Plugin:      &agentrail.PluginInvocation{BindingID: bindingID, InvocationID: "unassigned"},
	}})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	all := spans(r)
	if strings.Contains(fmt.Sprint(r.traces), "synthetic-private-operation-attempt") {
		t.Fatal("raw operation reference exported")
	}
	first := spanNamed(t, all, "delivery.requested")
	for k, want := range map[string]string{
		"agentrail.operation.namespace_id": namespaceID,
		"agentrail.operation.hash":         hash,
		"agentrail.environment":            "production",
		"agentrail.workflow.role":          "start",
		"agentrail.workflow.event_name":    "delivery.requested",
		"agentrail.plugin.binding_id":      bindingID,
		"agentrail.plugin.invocation_id":   "invocation-1",
	} {
		if got := stringAttr(first, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := stringAttr(spanNamed(t, all, "delivery.completed"), "agentrail.workflow.role"); got != "terminal" {
		t.Errorf("terminal role = %q", got)
	}
	if stringAttr(spanNamed(t, all, "delivery.completed"), "agentrail.operation.hash") != hash {
		t.Error("one attempt's start and terminal hash differently")
	}
	for _, name := range []string{"next.attempt", "other.namespace"} {
		if got := stringAttr(spanNamed(t, all, name), "agentrail.operation.hash"); got == "" || got == hash {
			t.Errorf("%s shares the first attempt's hash", name)
		}
	}
	child := spanNamed(t, all, "child.step")
	if string(child.ParentSpanId) != string(first.SpanId) {
		t.Fatal("child step is not nested under the evidence milestone")
	}
	for _, a := range child.Attributes {
		if strings.HasPrefix(a.Key, "agentrail.operation") || strings.HasPrefix(a.Key, "agentrail.plugin") || strings.HasPrefix(a.Key, "agentrail.workflow") || a.Key == "agentrail.environment" {
			t.Errorf("child span inherited %s", a.Key)
		}
	}
	standalone := spanNamed(t, all, "plugin.invoked")
	if stringAttr(standalone, "agentrail.environment") != "test" || stringAttr(standalone, "agentrail.plugin.invocation_id") != "unassigned" {
		t.Error("plugin-only evidence missing")
	}
	if attr(standalone.Attributes, "agentrail.operation.hash") != nil || attr(standalone.Attributes, "agentrail.workflow.role") != nil {
		t.Error("plugin-only invocation exported as a workflow outcome")
	}
}

// Fails if evidence the collector would refuse is exported, or if invalid
// evidence stops the milestone or the application's callback.
func TestInvalidPerformanceIsOmittedAndReported(t *testing.T) {
	op := func(ns, ref string, role agentrail.WorkflowRole) *agentrail.AuthorizedOperation {
		return &agentrail.AuthorizedOperation{NamespaceID: ns, InstanceRef: ref, Role: role}
	}
	plugin := &agentrail.PluginInvocation{BindingID: bindingID, InvocationID: "invocation-1"}
	cases := map[string]agentrail.PerformanceContext{
		"unknown environment":     {Environment: "staging", Plugin: plugin},
		"nothing to report":       {Environment: agentrail.EnvironmentProduction},
		"operation without event": {Environment: agentrail.EnvironmentProduction, Operation: op(namespaceID, "a", agentrail.RoleStart)},
		"event without operation": {Environment: agentrail.EnvironmentProduction, EventName: "x", Plugin: plugin},
		"non-canonical namespace": {Environment: agentrail.EnvironmentProduction, Operation: op("11111111-1111-1111-1111-111111111111", "a", agentrail.RoleStart), EventName: "x"},
		"uppercase namespace":     {Environment: agentrail.EnvironmentProduction, Operation: op("AAAAAAAA-1111-4111-8111-111111111111", "a", agentrail.RoleStart), EventName: "x"},
		"empty reference":         {Environment: agentrail.EnvironmentProduction, Operation: op(namespaceID, "", agentrail.RoleStart), EventName: "x"},
		"invalid UTF-8 reference": {Environment: agentrail.EnvironmentProduction, Operation: op(namespaceID, "\xff", agentrail.RoleStart), EventName: "x"},
		"unknown role":            {Environment: agentrail.EnvironmentProduction, Operation: op(namespaceID, "a", "finished"), EventName: "x"},
		"event name with space":   {Environment: agentrail.EnvironmentProduction, Operation: op(namespaceID, "a", agentrail.RoleStart), EventName: "delivery requested"},
		"event name too long":     {Environment: agentrail.EnvironmentProduction, Operation: op(namespaceID, "a", agentrail.RoleStart), EventName: "a" + strings.Repeat("b", 120)},
		"bad binding":             {Environment: agentrail.EnvironmentProduction, Plugin: &agentrail.PluginInvocation{BindingID: "binding", InvocationID: "i"}},
		"empty invocation":        {Environment: agentrail.EnvironmentProduction, Plugin: &agentrail.PluginInvocation{BindingID: bindingID}},
		"invocation over 256":     {Environment: agentrail.EnvironmentProduction, Plugin: &agentrail.PluginInvocation{BindingID: bindingID, InvocationID: strings.Repeat("😀", 129)}},
		"invocation with NUL":     {Environment: agentrail.EnvironmentProduction, Plugin: &agentrail.PluginInvocation{BindingID: bindingID, InvocationID: "a\x00"}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			r, s := collector(t)
			var mu sync.Mutex
			codes := []string{}
			o := options(s.URL)
			o.OnError = func(_ context.Context, e agentrail.CaptureError) {
				mu.Lock()
				defer mu.Unlock()
				codes = append(codes, e.Code)
			}
			c, err := agentrail.New(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Shutdown(context.Background())
			ran := false
			err = c.WithMilestone(context.Background(), "checkout", agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "business.event", Performance: &p}, func(context.Context) error {
				ran = true
				return nil
			})
			if err != nil || !ran {
				t.Fatal("invalid evidence interrupted the application callback")
			}
			if err := c.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if fmt.Sprint(codes) != "[invalid_performance]" {
				t.Errorf("reported %v", codes)
			}
			mu.Unlock()
			r.mu.Lock()
			defer r.mu.Unlock()
			sp := spanNamed(t, spans(r), "checkout")
			if attr(sp.Attributes, "agentrail.step") == nil {
				t.Error("milestone lost its step")
			}
			for _, a := range sp.Attributes {
				if strings.HasPrefix(a.Key, "agentrail.operation") || strings.HasPrefix(a.Key, "agentrail.plugin") || strings.HasPrefix(a.Key, "agentrail.workflow") || a.Key == "agentrail.environment" {
					t.Errorf("exported %s from invalid evidence", a.Key)
				}
			}
		})
	}
}
