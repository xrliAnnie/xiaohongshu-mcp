package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

var boundaryFixtureHostAcceptance = false

var boundaryFixtureManifest = strings.Repeat("1", 64)
var boundaryFixtureBootstrap = strings.Repeat("2", 64)

func TestGuardedBoundaryRequiresSignedMatchingMeasurements(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config, binary, schema := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	probe := strings.Repeat("e", 64)
	statement := guardedBoundaryStatement{HostAcceptance: &boundaryFixtureHostAcceptance, NotCovered: boundaryFixtureNotCovered, ManifestSHA256: boundaryFixtureManifest, BootstrapSHA256: boundaryFixtureBootstrap, ProbeKind: "fixture_harness", ProbeSHA256: probe, SchemaVersion: 1, ConfigDigest: config, ProviderBinarySHA256: binary, ToolSchemaDigest: schema, Passed: true}
	payload := canonicalBoundaryStatement(statement)
	envelope := guardedBoundaryEnvelope{Statement: statement, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(boundarySignatureDomain), payload...)))}
	raw, _ := json.Marshal(envelope)
	if verifyGuardedBoundary(raw, pub, config, binary, schema, probe, boundaryFixtureManifest, boundaryFixtureBootstrap) != nil {
		t.Fatal("valid signed fixture rejected")
	}
	for _, m := range [][3]string{{strings.Repeat("d", 64), binary, schema}, {config, strings.Repeat("d", 64), schema}, {config, binary, strings.Repeat("d", 64)}} {
		if verifyGuardedBoundary(raw, pub, m[0], m[1], m[2], probe, boundaryFixtureManifest, boundaryFixtureBootstrap) == nil {
			t.Fatal("measurement mismatch accepted")
		}
	}
	if verifyGuardedBoundary(raw, pub, config, binary, schema, strings.Repeat("f", 64), boundaryFixtureManifest, boundaryFixtureBootstrap) == nil {
		t.Fatal("probe digest mismatch accepted")
	}
	envelope.Statement.Passed = false
	payload = canonicalBoundaryStatement(envelope.Statement)
	envelope.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(boundarySignatureDomain), payload...)))
	raw, _ = json.Marshal(envelope)
	if verifyGuardedBoundary(raw, pub, config, binary, schema, probe, boundaryFixtureManifest, boundaryFixtureBootstrap) == nil {
		t.Fatal("signed failed probes accepted")
	}
	envelope.Statement.Passed = true
	raw, _ = json.Marshal(envelope)
	if verifyGuardedBoundary(raw, pub, config, binary, schema, probe, boundaryFixtureManifest, boundaryFixtureBootstrap) == nil {
		t.Fatal("changed verdict retained signature")
	}
}
func TestGuardedBoundaryRejectsMissingReceipt(t *testing.T) {
	if verifyGuardedBoundary(nil, make([]byte, 32), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("e", 64), boundaryFixtureManifest, boundaryFixtureBootstrap) == nil {
		t.Fatal("missing proof accepted")
	}
}

func TestGuardedBoundaryRequiresExplicitFixtureClassification(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	config, binary, schema, probe := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("e", 64)
	for _, kind := range []string{"fixture_harness", "", "real_platform"} {
		statement := map[string]any{"hostAcceptance": false, "notCovered": boundaryFixtureNotCovered, "manifestSha256": boundaryFixtureManifest, "bootstrapSha256": boundaryFixtureBootstrap, "schemaVersion": 1, "configDigest": config, "providerBinarySha256": binary, "toolSchemaDigest": schema, "probeSha256": probe, "passed": true}
		if kind != "" {
			statement["probeKind"] = kind
		}
		payload, _ := json.Marshal(statement)
		raw, _ := json.Marshal(map[string]any{"statement": statement, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte(boundarySignatureDomain), payload...)))})
		got := verifyGuardedBoundary(raw, pub, config, binary, schema, probe, boundaryFixtureManifest, boundaryFixtureBootstrap)
		if (got == nil) != (kind == "fixture_harness") {
			t.Fatalf("kind %q verification: %v", kind, got)
		}
	}
}

func TestGuardedBoundarySharedInstallationFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/boundary-installation.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Expected map[string]string `json:"expected"`
		Envelope json.RawMessage   `json:"envelope"`
	}
	if json.Unmarshal(raw, &f) != nil {
		t.Fatal("bad fixture")
	}
	key, err := base64.StdEncoding.DecodeString(f.Expected["publicKey"])
	if err != nil {
		t.Fatal(err)
	}
	check := func(manifest, bootstrap string) error {
		return verifyGuardedBoundary(f.Envelope, key, f.Expected["configDigest"], f.Expected["providerBinarySha256"], f.Expected["toolSchemaDigest"], f.Expected["probeSha256"], manifest, bootstrap)
	}
	if check(f.Expected["manifestSha256"], f.Expected["bootstrapSha256"]) != nil {
		t.Fatal("shared TS signature refused")
	}
	if check(strings.Repeat("9", 64), f.Expected["bootstrapSha256"]) == nil || check(f.Expected["manifestSha256"], strings.Repeat("9", 64)) == nil {
		t.Fatal("installation drift accepted")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(f.Envelope, &envelope) != nil {
		t.Fatal("bad envelope")
	}
	for _, name := range []string{"manifestSha256", "bootstrapSha256", "hostAcceptance", "notCovered"} {
		var statement map[string]any
		if json.Unmarshal(envelope["statement"], &statement) != nil {
			t.Fatal("bad statement")
		}
		delete(statement, name)
		pub, private, _ := ed25519.GenerateKey(rand.Reader)
		payload, _ := json.Marshal(statement)
		legacy, _ := json.Marshal(map[string]any{"statement": statement, "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(private, append([]byte(boundarySignatureDomain), payload...)))})
		if verifyGuardedBoundary(legacy, pub, f.Expected["configDigest"], f.Expected["providerBinarySha256"], f.Expected["toolSchemaDigest"], f.Expected["probeSha256"], f.Expected["manifestSha256"], f.Expected["bootstrapSha256"]) == nil {
			t.Fatal("missing installation field accepted", name)
		}
	}
}

func TestGuardedBoundaryRejectsSignedHostAcceptanceClaims(t *testing.T) {
	raw, err := os.ReadFile("testdata/boundary-installation.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Expected map[string]string
		Envelope struct{ Statement map[string]any }
	}
	if json.Unmarshal(raw, &f) != nil {
		t.Fatal("bad fixture")
	}
	for _, change := range []string{"true", "short-scope", "missing-with-retained-signature"} {
		t.Run(change, func(t *testing.T) {
			statement := make(map[string]any)
			for k, v := range f.Envelope.Statement {
				statement[k] = v
			}
			pub, private, _ := ed25519.GenerateKey(rand.Reader)
			if change == "true" {
				statement["hostAcceptance"] = true
			}
			if change == "short-scope" {
				statement["notCovered"] = []string{"legacy_cutover"}
			}
			payload, _ := json.Marshal(statement)
			signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, append([]byte(boundarySignatureDomain), payload...)))
			if change == "missing-with-retained-signature" {
				delete(statement, "hostAcceptance")
			}
			altered, _ := json.Marshal(map[string]any{"statement": statement, "signature": signature})
			e := f.Expected
			if verifyGuardedBoundary(altered, pub, e["configDigest"], e["providerBinarySha256"], e["toolSchemaDigest"], e["probeSha256"], e["manifestSha256"], e["bootstrapSha256"]) == nil {
				t.Fatal("ambiguous host claim accepted")
			}
		})
	}
}
