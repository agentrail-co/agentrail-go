package agentrail

import (
	"encoding/json"
	"os"
	"testing"
)

// This fails if a payload or batch limit drifts from the contract's
// otlp.limits, which the collector and the other SDKs hold to.
func TestLimitsMatchTheContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		OTLP struct {
			Limits struct {
				MCPPayloadBytes  int `json:"mcpPayloadBytes"`
				ExportBatchBytes int `json:"exportBatchBytes"`
			} `json:"limits"`
		} `json:"otlp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if recordPayloadBytes != c.OTLP.Limits.MCPPayloadBytes || batchBytes != c.OTLP.Limits.ExportBatchBytes {
		t.Fatalf("payload %d, batch %d; contract %+v", recordPayloadBytes, batchBytes, c.OTLP.Limits)
	}
}
