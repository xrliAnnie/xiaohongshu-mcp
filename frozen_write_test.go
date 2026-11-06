package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type frozenVector struct {
	Canonical, Digest string
	Now               int64
}

func frozenVectors(t *testing.T) []frozenVector {
	t.Helper()
	b, e := os.ReadFile("testdata/frozen-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var v []frozenVector
	if json.Unmarshal(b, &v) != nil {
		t.Fatal("bad vectors")
	}
	return v
}
func TestFrozenWriteCrossLanguageVectors(t *testing.T) {
	for _, v := range frozenVectors(t) {
		if _, err := verifyFrozenWrite([]byte(v.Canonical), v.Digest, v.Now); err != nil {
			t.Fatal(err)
		}
	}
}
func TestFrozenWriteRejectsChangedOrAmbiguousWire(t *testing.T) {
	v := frozenVectors(t)[0]
	for _, raw := range []string{" " + v.Canonical, strings.Replace(v.Canonical, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1), strings.Replace(v.Canonical, "标题", "changed", 1), strings.Replace(v.Canonical, `"accountEpoch":1`, `"accountEpoch":1.0`, 1)} {
		if _, err := verifyFrozenWrite([]byte(raw), v.Digest, v.Now); err == nil {
			t.Fatal("accepted changed wire")
		}
	}
}

func TestFrozenWriteSchemaRejectsEvenWithMatchingDigest(t *testing.T) {
	v := frozenVectors(t)[0]
	for _, test := range []struct {
		name   string
		mutate func(*frozenWrite)
	}{
		{"uuid version", func(w *frozenWrite) { w.ProposalID = "d95c13f3-0057-fae7-a654-456eef4ee882" }},
		{"unknown operation", func(w *frozenWrite) { w.OperationID = "xiaohongshu.publish_new" }},
		{"epoch zero", func(w *frozenWrite) { w.Account.AccountEpoch = 0 }},
		{"target on publish", func(w *frozenWrite) { w.Target = &frozenTarget{FeedID: "feed"} }},
		{"missing original", func(w *frozenWrite) { w.Payload.IsOriginal = nil }},
		{"missing media", func(w *frozenWrite) { w.Media = []frozenArtifact{} }},
		{"media URL", func(w *frozenWrite) { w.Media[0].MIMEType = "text/html" }},
		{"video mismatch", func(w *frozenWrite) { w.OperationID = "xiaohongshu.publish_with_video" }},
		{"title units", func(w *frozenWrite) { s := strings.Repeat("中", 21); w.Payload.Title = &s }},
		{"empty content", func(w *frozenWrite) { s := ""; w.Payload.Content = &s }},
		{"unknown visibility", func(w *frozenWrite) { s := "everyone"; w.Payload.Visibility = &s }},
		{"past schedule", func(w *frozenWrite) { s := "2026-09-14T00:00:00.000Z"; w.Payload.ScheduleAt = &s }},
		{"nil tags", func(w *frozenWrite) { w.Payload.Tags = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var w frozenWrite
			if json.Unmarshal([]byte(v.Canonical), &w) != nil {
				t.Fatal("fixture")
			}
			test.mutate(&w)
			raw, err := canonicalFrozenWrite(w)
			if err != nil {
				return
			}
			sum := sha256.Sum256(append([]byte("flywheel:xhs-write:v1\n"), raw...))
			if _, err = verifyFrozenWrite(raw, hex.EncodeToString(sum[:]), v.Now); err == nil {
				t.Fatal("accepted invalid schema")
			}
		})
	}
}
