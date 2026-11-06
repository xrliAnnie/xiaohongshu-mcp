package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

const permitPurpose = "flywheel:xhs-dispatch:v1"
const permitDomain = "flywheel:xhs-permit:v1\n"
const maxPermitInteger int64 = 9007199254740991

var errWritePermit = errors.New("write_permit_invalid")

type internalPermit struct {
	Purpose            string `json:"purpose"`
	Issuer             string `json:"issuer"`
	Audience           string `json:"audience"`
	ProposalID         string `json:"proposalId"`
	ReceiptID          string `json:"receiptId"`
	AttemptID          string `json:"attemptId"`
	ContentDigest      string `json:"contentDigest"`
	AccountUserID      string `json:"accountUserId"`
	AccountEpoch       int64  `json:"accountEpoch"`
	ProviderGeneration string `json:"providerGeneration"`
	LeaseID            string `json:"leaseId"`
	IssuedAt           int64  `json:"issuedAt"`
	ExpiresAt          int64  `json:"expiresAt"`
	KeyID              string `json:"keyId"`
}
type permitExpectation struct {
	Audience, KeyID, ProposalID, ContentDigest, AccountUserID, Generation, LeaseID string
	AccountEpoch, LeaseExpiresAt                                                   int64
}

// Successful verification alone is NOT dispatch admission: account recheck, authority
// admission and durable tombstone consumption are still required by the private commit handler.
func verifyInternalPermit(raw []byte, signature string, key []byte, expected permitExpectation, now int64) (internalPermit, error) {
	var p internalPermit
	invalid := func() (internalPermit, error) { return internalPermit{}, errWritePermit }
	if len(raw) > 8192 || len(key) != 32 || !utf8.Valid(raw) || now < 0 || now > maxPermitInteger {
		return invalid()
	}
	if json.Unmarshal(raw, &p) != nil {
		return invalid()
	}
	encoded, err := canonicalInternalPermit(p)
	// Authority transmits the exact canonical string. This also rejects unknown/missing
	// fields, duplicate keys, invalid surrogate escapes and parser-dependent numeric encodings.
	if err != nil || !bytes.Equal(raw, encoded) {
		return invalid()
	}
	if p.Purpose != permitPurpose || p.Issuer != "xhs-authority" || p.Audience != expected.Audience ||
		p.KeyID != expected.KeyID || p.ProposalID != expected.ProposalID || p.ContentDigest != expected.ContentDigest ||
		p.AccountUserID != expected.AccountUserID || p.AccountEpoch != expected.AccountEpoch ||
		p.ProviderGeneration != expected.Generation || p.LeaseID != expected.LeaseID ||
		!journalDigest.MatchString(p.ContentDigest) || p.AccountEpoch < 0 || p.AccountEpoch > maxPermitInteger ||
		p.IssuedAt < 0 || p.IssuedAt > now || p.ExpiresAt <= now || p.ExpiresAt > maxPermitInteger ||
		p.ExpiresAt > p.IssuedAt+60000 || p.ExpiresAt > expected.LeaseExpiresAt {
		return invalid()
	}
	if len(signature) != 64 {
		return invalid()
	}
	supplied, err := hex.DecodeString(signature)
	if err != nil || hex.EncodeToString(supplied) != signature {
		return invalid()
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(permitDomain))
	mac.Write(encoded)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return invalid()
	}
	return p, nil
}

// Keys here are fixed ASCII, so byte sorting is identical to the TS UTF-16 key order.
// String encoding preserves Unicode (including U+2028/2029) and never HTML-escapes it.
func canonicalInternalPermit(p internalPermit) ([]byte, error) {
	fields := map[string]any{
		"purpose": p.Purpose, "issuer": p.Issuer, "audience": p.Audience, "proposalId": p.ProposalID,
		"receiptId": p.ReceiptID, "attemptId": p.AttemptID, "contentDigest": p.ContentDigest,
		"accountUserId": p.AccountUserID, "accountEpoch": p.AccountEpoch, "providerGeneration": p.ProviderGeneration,
		"leaseId": p.LeaseID, "issuedAt": p.IssuedAt, "expiresAt": p.ExpiresAt, "keyId": p.KeyID,
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result bytes.Buffer
	result.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			result.WriteByte(',')
		}
		appendPermitString(&result, key)
		result.WriteByte(':')
		switch value := fields[key].(type) {
		case string:
			if value == "" || len(utf16.Encode([]rune(value))) > 256 || !utf8.ValidString(value) {
				return nil, errWritePermit
			}
			appendPermitString(&result, value)
		case int64:
			if value < 0 || value > maxPermitInteger {
				return nil, errWritePermit
			}
			result.WriteString(strconv.FormatInt(value, 10))
		}
	}
	result.WriteByte('}')
	return result.Bytes(), nil
}
func appendPermitString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(r)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(output, `\u%04x`, r)
			} else {
				output.WriteRune(r)
			}
		}
	}
	output.WriteByte('"')
}
