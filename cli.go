// Copyright 2026 Agentrail. Licensed under the Apache License, Version 2.0.
package agentrail

import (
	"regexp"
	"strings"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
	"github.com/dunglas/httpsfv"
	"go.opentelemetry.io/otel/attribute"
)

// cliHeaderName is the header a customer's CLI sends its own API, written
// by agentrail-cli-sdk: the command an agent ran, the run, the session and
// the agent. Its contract is sdk-contract.json's cliHeader.
const cliHeaderName = "X-Agentrail-Cli"

var (
	cliText     = regexp.MustCompile(`^[\x20-\x7e]{1,64}$`)
	cliCommand  = regexp.MustCompile(`^[\x20-\x7e]{1,128}$`)
	cliRun      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	cliSession  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	cliAgent    = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
	cliVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// The largest Integer RFC 9651 holds, and the most evidence a header names.
const (
	cliMaxInteger  = 999_999_999_999_999
	cliMaxEvidence = 8
)

// cliHeader is what the header says: an empty optional string is one the
// header did not carry (every member it carries holds at least one
// character).
type cliHeader struct {
	sdkVersion, cli, cliVersion, run, session, sessionFrom, mode string
	agent, agentVersion, command                                 string
	sequence                                                     int64
	evidence                                                     []string
}

// topLevelMembers counts a Dictionary's members as written: the commas
// outside quoted strings, plus one. Only a string (or a display string)
// can hold a comma, so for a value that parsed this is exact, and a count
// past the parsed names means a key given twice.
func topLevelMembers(value string) int {
	members, quoted, escaped := 1, false, false
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case !quoted && c == ',':
			members++
		}
	}
	return members
}

func itemOf(d *httpsfv.Dictionary, key string) (any, bool) {
	member, ok := d.Get(key)
	if !ok {
		return nil, false
	}
	item, isItem := member.(httpsfv.Item)
	if !isItem {
		return nil, false
	}
	return item.Value, true
}

// stringOf is a member that must be a String matching pattern: absent is
// allowed when optional, any other type or text is not.
func stringOf(d *httpsfv.Dictionary, key string, pattern *regexp.Regexp, optional bool) (string, bool) {
	if _, present := d.Get(key); !present {
		return "", optional
	}
	value, isItem := itemOf(d, key)
	text, isString := value.(string)
	if !isItem || !isString || !pattern.MatchString(text) {
		return "", false
	}
	return text, true
}

func tokenOf(d *httpsfv.Dictionary, key string, allowed ...string) (string, bool) {
	value, _ := itemOf(d, key)
	token, isToken := value.(httpsfv.Token)
	if !isToken {
		return "", false
	}
	for _, candidate := range allowed {
		if string(token) == candidate {
			return candidate, true
		}
	}
	return "", false
}

// readCLIHeader reads the request's header as agentrail-core's readCliHeader
// does. It reads nothing from a request that sent it more than once, a
// value that is not a Dictionary, misses a member, holds one outside the
// contract's rules, or gives one twice. A later version only adds members:
// any v from 1 is read, and members this one does not know are ignored.
func readCLIHeader(values []string) (cliHeader, bool) {
	if len(values) != 1 {
		return cliHeader{}, false
	}
	d, err := httpsfv.UnmarshalDictionary(values)
	if err != nil || len(d.Names()) != topLevelMembers(values[0]) {
		return cliHeader{}, false
	}
	version, _ := itemOf(d, "v")
	if v, isInteger := version.(int64); !isInteger || v < 1 {
		return cliHeader{}, false
	}
	var h cliHeader
	checks := []bool{}
	read := func(target *string, key string, pattern *regexp.Regexp, optional bool) {
		value, ok := stringOf(d, key, pattern, optional)
		*target = value
		checks = append(checks, ok)
	}
	read(&h.sdkVersion, "sdk", cliText, false)
	read(&h.cli, "cli", cliText, false)
	read(&h.cliVersion, "cliv", cliText, true)
	read(&h.run, "run", cliRun, false)
	read(&h.session, "ses", cliSession, false)
	read(&h.agent, "agent", cliAgent, true)
	read(&h.agentVersion, "agentv", cliText, true)
	read(&h.command, "cmd", cliCommand, true)
	var ok bool
	h.sessionFrom, ok = tokenOf(d, "sesfrom", "agent", "ci", "local", "run")
	checks = append(checks, ok)
	h.mode, ok = tokenOf(d, "mode", "agent", "ci", "interactive", "non-interactive")
	checks = append(checks, ok)
	sequence, _ := itemOf(d, "seq")
	seq, isInteger := sequence.(int64)
	checks = append(checks, isInteger && seq >= 1 && seq <= cliMaxInteger)
	h.sequence = seq
	if member, present := d.Get("ev"); present {
		list, isList := member.(httpsfv.InnerList)
		ok = isList && len(list.Items) >= 1 && len(list.Items) <= cliMaxEvidence
		for _, item := range list.Items {
			name, isString := item.Value.(string)
			ok = ok && isString && cliVariable.MatchString(name)
			h.evidence = append(h.evidence, name)
		}
		checks = append(checks, ok)
	}
	for _, passed := range checks {
		if !passed {
			return cliHeader{}, false
		}
	}
	return h, true
}

// cliAttributes are what the header says, as the span attributes the
// collector reads (otlp.cliAttributes). The session leaves only as its keyed
// hash, as a principal's id does.
func cliAttributes(hashKey string, h cliHeader) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("agentrail.cli.name", h.cli),
		attribute.String("agentrail.cli.run_id", h.run),
		attribute.Int64("agentrail.cli.sequence", h.sequence),
		attribute.String("agentrail.cli.mode", h.mode),
		attribute.String("agentrail.cli.session.id_hash", handoff.Hash(hashKey, h.session)),
		attribute.String("agentrail.cli.session.source", h.sessionFrom),
		attribute.String("agentrail.cli.sdk.version", h.sdkVersion),
	}
	for _, optional := range []struct{ key, value string }{
		{"agentrail.cli.version", h.cliVersion},
		{"agentrail.cli.command", h.command},
		{"agentrail.cli.agent.name", h.agent},
		{"agentrail.cli.agent.version", h.agentVersion},
		{"agentrail.cli.agent.evidence", strings.Join(h.evidence, ",")},
	} {
		if optional.value != "" {
			attrs = append(attrs, attribute.String(optional.key, optional.value))
		}
	}
	return attrs
}
