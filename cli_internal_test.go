// Copyright 2026 Agentrail. Licensed under the Apache License, Version 2.0.
package agentrail

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/agentrail-co/agentrail-go/internal/handoff"
)

type cliReads struct {
	SDKVersion   string   `json:"sdkVersion"`
	CLI          string   `json:"cli"`
	CLIVersion   string   `json:"cliVersion"`
	Run          string   `json:"run"`
	Sequence     int64    `json:"sequence"`
	Session      string   `json:"session"`
	SessionFrom  string   `json:"sessionFrom"`
	Mode         string   `json:"mode"`
	Agent        string   `json:"agent"`
	AgentVersion string   `json:"agentVersion"`
	Evidence     []string `json:"evidence"`
	Command      string   `json:"command"`
}

type cliVectors struct {
	Header struct {
		Value string   `json:"value"`
		Reads cliReads `json:"reads"`
	} `json:"header"`
	Later struct {
		Value string   `json:"value"`
		Reads cliReads `json:"reads"`
	} `json:"later"`
	Unread       []string `json:"unread"`
	KeyedSession struct {
		HashKey string `json:"hashKey"`
		Session string `json:"session"`
		Hash    string `json:"hash"`
	} `json:"keyedSession"`
}

func contractCLIVectors(t *testing.T) cliVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/sdk-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		CLIHeader struct {
			Vectors cliVectors `json:"vectors"`
		} `json:"cliHeader"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.CLIHeader.Vectors
}

func readsOf(h cliHeader) cliReads {
	return cliReads{h.sdkVersion, h.cli, h.cliVersion, h.run, h.sequence, h.session, h.sessionFrom, h.mode, h.agent, h.agentVersion, h.evidence, h.command}
}

// This fails if the Go reader and the contract part: a header agentrail-cli-sdk
// wrote would read as another command or session here than in Node, or a
// header no CLI SDK writes (a raw session id, a header sent twice and joined)
// would reach the collector as a session.
func TestCLIHeaderReadsAsTheContractSays(t *testing.T) {
	v := contractCLIVectors(t)
	for name, vector := range map[string]struct {
		value string
		reads cliReads
	}{"header": {v.Header.Value, v.Header.Reads}, "later": {v.Later.Value, v.Later.Reads}} {
		h, ok := readCLIHeader([]string{vector.value})
		if !ok || !reflect.DeepEqual(readsOf(h), vector.reads) {
			t.Errorf("%s: read %v %+v, want %+v", name, ok, readsOf(h), vector.reads)
		}
	}
	if len(v.Unread) == 0 {
		t.Fatal("the contract names no header to refuse")
	}
	for _, value := range v.Unread {
		if h, ok := readCLIHeader([]string{value}); ok {
			t.Errorf("read %q as %+v", value, readsOf(h))
		}
	}
	if _, ok := readCLIHeader([]string{v.Header.Value, v.Header.Value}); ok {
		t.Error("read a header sent twice")
	}
	if got := handoff.Hash(v.KeyedSession.HashKey, v.KeyedSession.Session); got != v.KeyedSession.Hash {
		t.Errorf("keyed session %s, want %s", got, v.KeyedSession.Hash)
	}
}
