// Package coexistence tests Agentrail beside OpenTelemetry's otelhttp, Sentry
// and Datadog, and holds the beside-otel example. It is a module of its own so
// that their dependencies never enter agentrail-go's go.mod, and so never a
// customer's build. Run its tests from this directory.
package coexistence
