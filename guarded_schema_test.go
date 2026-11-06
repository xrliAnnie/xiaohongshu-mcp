package main

import (
	"context"
	"testing"
)

func TestGuardedSchemaMeasuresRegisteredCatalogWithoutBusinessCalls(t *testing.T) {
	tools, err := registeredGuardedTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 16 {
		t.Fatal("unexpected catalog size", len(tools))
	}
	before, err := guardedCatalogDigest(tools)
	if err != nil || !journalDigest.MatchString(before) {
		t.Fatal("digest", err)
	}
	tools[0].Description += " changed"
	after, err := guardedCatalogDigest(tools)
	if err != nil || after == before {
		t.Fatal("catalog drift invisible")
	}
	tools[1].Name = tools[0].Name
	if _, err = guardedCatalogDigest(tools); err == nil {
		t.Fatal("duplicate tool identity")
	}
}
