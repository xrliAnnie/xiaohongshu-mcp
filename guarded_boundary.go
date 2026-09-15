package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
)

const boundarySignatureDomain = "flywheel:xhs-boundary:v1\n"

type guardedBoundaryStatement struct {
	SchemaVersion        int    `json:"schemaVersion"`
	ConfigDigest         string `json:"configDigest"`
	ProviderBinarySHA256 string `json:"providerBinarySha256"`
	ToolSchemaDigest     string `json:"toolSchemaDigest"`
	ProbeSHA256          string `json:"probeSha256"`
	ProbeKind            string `json:"probeKind"`
	Passed               bool   `json:"passed"`
}
type guardedBoundaryEnvelope struct {
	Statement guardedBoundaryStatement `json:"statement"`
	Signature string                   `json:"signature"`
}

// All keys and values here are fixed ASCII protocol fields. The root QA signer
// uses the same lexicographic JSON and signature domain, after actual host probes.
func canonicalBoundaryStatement(s guardedBoundaryStatement) []byte {
	raw, _ := json.Marshal(map[string]any{"schemaVersion": s.SchemaVersion, "configDigest": s.ConfigDigest, "providerBinarySha256": s.ProviderBinarySHA256, "toolSchemaDigest": s.ToolSchemaDigest, "passed": s.Passed, "probeSha256": s.ProbeSHA256, "probeKind": s.ProbeKind})
	return raw
}

// publicKey and configDigest must come from the immutable root-owned startup
// policy; measured binary/schema digests must be computed locally. None may be
// supplied by a model or an ingress request. This function never issues proofs.
func verifyGuardedBoundary(raw, publicKey []byte, configDigest, binaryDigest, schemaDigest, probeDigest string) error {
	if len(raw) > 4096 || len(publicKey) != ed25519.PublicKeySize || !journalDigest.MatchString(configDigest) || !journalDigest.MatchString(binaryDigest) || !journalDigest.MatchString(schemaDigest) || !journalDigest.MatchString(probeDigest) {
		return errPrivateProvider
	}
	var envelope guardedBoundaryEnvelope
	if !strictAuthorityJSON(raw, &envelope) {
		return errPrivateProvider
	}
	statement := envelope.Statement
	if statement.SchemaVersion != 1 || statement.ProbeKind != "fixture_harness" || !statement.Passed || statement.ConfigDigest != configDigest || statement.ProviderBinarySHA256 != binaryDigest || statement.ToolSchemaDigest != schemaDigest || statement.ProbeSHA256 != probeDigest {
		return errPrivateProvider
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, append([]byte(boundarySignatureDomain), canonicalBoundaryStatement(statement)...), signature) {
		return errPrivateProvider
	}
	return nil
}
