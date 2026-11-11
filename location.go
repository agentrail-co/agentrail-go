package agentrail

import (
	"net/http"
	"regexp"

	"go.opentelemetry.io/otel/attribute"
)

var locationCountry = regexp.MustCompile(`^[A-Z]{2}$`)

// Capture an owned, sanitized snapshot before the handler or its child goroutines run.
func (c *Client) captureLocation(r *http.Request) (attrs []attribute.KeyValue) {
	if c.options.IncomingLocation == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			attrs = nil
			c.report(r.Context(), "location_failed", "context", 1)
		}
	}()
	location, err := c.options.IncomingLocation(r)
	if err != nil {
		c.report(r.Context(), "location_failed", "context", 1)
		return nil
	}
	if location.Country != "" && !locationCountry.MatchString(location.Country) {
		c.report(r.Context(), "location_failed", "context", 1)
		return nil
	}
	if location.Source == "" {
		location.Source = LocationConfigured
	}
	switch location.Source {
	case LocationDBIP, LocationCloudflare, LocationVercel, LocationConfigured:
	default:
		c.report(r.Context(), "location_failed", "context", 1)
		return nil
	}
	for _, field := range []struct{ name, value string }{
		{"country", location.Country}, {"region", location.Region}, {"city", location.City},
	} {
		if value := bounded(field.value, 128); value != "" {
			attrs = append(attrs, attribute.String("agentrail.client.location."+field.name, value))
		}
	}
	if len(attrs) > 0 {
		attrs = append(attrs, attribute.String("agentrail.client.location.source", string(location.Source)))
	}
	return attrs
}
