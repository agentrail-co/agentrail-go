// Copyright 2026 Agentrail. Licensed under the Apache License, Version 2.0.
package agentrail

import (
	"regexp"
	"unicode/utf8"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"go.opentelemetry.io/otel/attribute"
)

// PerformanceEnvironment separates production evidence from test evidence.
type PerformanceEnvironment string

const (
	EnvironmentProduction PerformanceEnvironment = "production"
	EnvironmentTest       PerformanceEnvironment = "test"
)

// WorkflowRole is where a milestone sits in one business attempt.
type WorkflowRole string

const (
	RoleStart    WorkflowRole = "start"
	RoleProgress WorkflowRole = "progress"
	RoleTerminal WorkflowRole = "terminal"
)

// AuthorizedOperation names a business attempt the application has already
// authorized. InstanceRef is hashed locally and never exported.
type AuthorizedOperation struct {
	NamespaceID, InstanceRef string
	Role                     WorkflowRole
}

// PluginInvocation names an authenticated plugin deployment and an opaque
// invocation ID, never tool arguments or a customer identifier.
type PluginInvocation struct{ BindingID, InvocationID string }

// PerformanceContext is explicit workflow or plugin evidence for one milestone.
// Supply an Operation with an EventName, a Plugin, or both; or a Plugin alone,
// without an EventName, for activity that earns no workflow-outcome credit.
type PerformanceContext struct {
	Environment PerformanceEnvironment
	Operation   *AuthorizedOperation
	EventName   string
	Plugin      *PluginInvocation
}

// Matches the collector's UUID contract, including version and variant.
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var businessEvent = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.:-]*$`)

// performanceAttributes returns only normalized, allowlisted evidence, or
// false when the evidence does not satisfy the collector's contract.
func performanceAttributes(p PerformanceContext, hashKey string) ([]attribute.KeyValue, bool) {
	if p.Environment != EnvironmentProduction && p.Environment != EnvironmentTest {
		return nil, false
	}
	if p.Operation == nil && (p.Plugin == nil || p.EventName != "") {
		return nil, false
	}
	if p.Operation != nil && p.Plugin == nil && p.EventName == "" {
		return nil, false
	}
	attrs := []attribute.KeyValue{attribute.String("agentrail.environment", string(p.Environment))}
	if p.Plugin != nil {
		if !canonicalUUID.MatchString(p.Plugin.BindingID) || p.Plugin.InvocationID == "" || !validText(p.Plugin.InvocationID, 256) {
			return nil, false
		}
		attrs = append(attrs, attribute.String("agentrail.plugin.binding_id", p.Plugin.BindingID), attribute.String("agentrail.plugin.invocation_id", p.Plugin.InvocationID))
	}
	if o := p.Operation; o != nil {
		if !canonicalUUID.MatchString(o.NamespaceID) || o.InstanceRef == "" || !utf8.ValidString(o.InstanceRef) {
			return nil, false
		}
		if o.Role != RoleStart && o.Role != RoleProgress && o.Role != RoleTerminal {
			return nil, false
		}
		attrs = append(attrs, attribute.String("agentrail.operation.namespace_id", o.NamespaceID), attribute.String("agentrail.operation.hash", handoff.OperationHash(hashKey, o.NamespaceID, o.InstanceRef)), attribute.String("agentrail.workflow.role", string(o.Role)))
		if p.EventName != "" {
			if len(p.EventName) > 120 || !businessEvent.MatchString(p.EventName) {
				return nil, false
			}
			attrs = append(attrs, attribute.String("agentrail.workflow.event_name", p.EventName))
		}
	}
	return attrs, true
}
