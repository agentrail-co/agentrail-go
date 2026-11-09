package agentrail

import (
	"context"
	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"go.opentelemetry.io/otel/attribute"
	"slices"
)

type PrincipalKind string

const (
	PrincipalAgentRegistration PrincipalKind = "agent_registration"
	PrincipalUser              PrincipalKind = "user"
	PrincipalAPIKey            PrincipalKind = "api_key"
	PrincipalAgentInstance     PrincipalKind = "agent_instance"
	PrincipalOAuthClient       PrincipalKind = "oauth_client"
)

type VerifiedIdentity struct {
	Kind             PrincipalKind
	ID, ActingUserID string
}

// Identify asserts identity the application has already authenticated. Headers,
// generic OTel user attributes and self-reported MCP metadata are never trusted.
func (c *Client) Identify(ctx context.Context, id VerifiedIdentity) error {
	if suppressed(ctx) {
		return nil
	}
	if err := c.ready(); err != nil {
		return err
	}
	if id.ID == "" || !validText(id.ID, 1<<20) || !validText(id.ActingUserID, 1<<20) {
		return &ConfigurationError{Problem: "identity IDs must be nonempty valid UTF-8 text"}
	}
	switch id.Kind {
	case PrincipalAgentRegistration, PrincipalUser, PrincipalAPIKey, PrincipalAgentInstance, PrincipalOAuthClient:
	default:
		return &ConfigurationError{Problem: "unsupported principal kind"}
	}
	s := c.state(ctx)
	if s == nil {
		c.report(ctx, "identity_outside_request", "context", 1)
		return nil
	}
	attrs := []attribute.KeyValue{attribute.Bool("agentrail.identity.verified", true), attribute.String("agentrail.principal.kind", string(id.Kind)), attribute.String("agentrail.principal.id_hash", handoff.Hash(c.options.HashKey, id.ID))}
	if id.ActingUserID != "" {
		attrs = append(attrs, attribute.String("agentrail.principal.acting_user_hash", handoff.Hash(c.options.HashKey, id.ActingUserID)))
	}
	s.mu.Lock()
	if s.identity == nil {
		s.identity = attrs
		s.mu.Unlock()
		return nil
	}
	same := slices.Equal(s.identity, attrs)
	s.mu.Unlock()
	if same {
		return nil
	}
	m := &mark{name: "identity.asserted", details: MilestoneContext{Surface: SurfaceAuth, Kind: "auth.identity"}, identity: attrs}
	_, span := c.tp.Tracer("agentrail-go").Start(context.WithValue(ctx, markKey{c}, m), m.name)
	span.End()
	return nil
}
