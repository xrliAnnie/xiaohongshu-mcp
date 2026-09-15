package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestGuardedBoundaryRequiresSignedMatchingMeasurements(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config, binary, schema := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	probe := strings.Repeat("e", 64)
	statement := guardedBoundaryStatement{ProbeKind: "fixture_harness", ProbeSHA256: probe, SchemaVersion: 1, ConfigDigest: config, ProviderBinarySHA256: binary, ToolSchemaDigest: schema, Passed: true}
	payload := canonicalBoundaryStatement(statement)
	envelope := guardedBoundaryEnvelope{Statement: statement, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(boundarySignatureDomain), payload...)))}
	raw, _ := json.Marshal(envelope)
	if verifyGuardedBoundary(raw, pub, config, binary, schema, probe) != nil {
		t.Fatal("valid signed fixture rejected")
	}
	for _, m := range [][3]string{{strings.Repeat("d", 64), binary, schema}, {config, strings.Repeat("d", 64), schema}, {config, binary, strings.Repeat("d", 64)}} {
		if verifyGuardedBoundary(raw, pub, m[0], m[1], m[2], probe) == nil {
			t.Fatal("measurement mismatch accepted")
		}
	}
	if verifyGuardedBoundary(raw, pub, config, binary, schema, strings.Repeat("f", 64)) == nil {
		t.Fatal("probe digest mismatch accepted")
	}
	envelope.Statement.Passed = false
	payload = canonicalBoundaryStatement(envelope.Statement)
	envelope.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(boundarySignatureDomain), payload...)))
	raw, _ = json.Marshal(envelope)
	if verifyGuardedBoundary(raw, pub, config, binary, schema, probe) == nil {
		t.Fatal("signed failed probes accepted")
	}
	envelope.Statement.Passed = true
	raw, _ = json.Marshal(envelope)
	if verifyGuardedBoundary(raw, pub, config, binary, schema, probe) == nil {
		t.Fatal("changed verdict retained signature")
	}
}
func TestGuardedBoundaryRejectsMissingReceipt(t *testing.T) {
	if verifyGuardedBoundary(nil, make([]byte, 32), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("e", 64)) == nil {
		t.Fatal("missing proof accepted")
	}
}

func TestGuardedBoundaryRequiresExplicitFixtureClassification(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	config, binary, schema, probe := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("e", 64)
	for _, kind := range []string{"fixture_harness", "", "real_platform"} {
		statement := map[string]any{"schemaVersion": 1, "configDigest": config, "providerBinarySha256": binary, "toolSchemaDigest": schema, "probeSha256": probe, "passed": true}
		if kind != "" {
			statement["probeKind"] = kind
		}
		payload, _ := json.Marshal(statement)
		raw, _ := json.Marshal(map[string]any{"statement": statement, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(boundarySignatureDomain), payload...)))})
		got := verifyGuardedBoundary(raw, pub, config, binary, schema, probe)
		if (got == nil) != (kind == "fixture_harness") {
			t.Fatalf("kind %q verification: %v", kind, got)
		}
	}
}
