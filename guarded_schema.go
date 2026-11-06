package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sort"
	"time"
)

// An in-memory MCP session lists the schemas registered by this exact binary.
// No business tool, browser, cookie loader or external transport is invoked.
func registeredGuardedTools(ctx context.Context) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	implementation := &mcp.Implementation{Name: "guarded-schema", Version: "1"}
	server := mcp.NewServer(implementation, nil)
	registerTools(server, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		return nil, errPrivateProvider
	}
	defer ss.Close()
	client := mcp.NewClient(implementation, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		return nil, errPrivateProvider
	}
	defer cs.Close()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil || listed.NextCursor != "" {
		return nil, errPrivateProvider
	}
	return listed.Tools, nil
}
func guardedCatalogDigest(tools []*mcp.Tool) (string, error) {
	if len(tools) != 16 {
		return "", errPrivateProvider
	}
	ordered := append([]*mcp.Tool(nil), tools...)
	for _, tool := range ordered {
		if tool == nil || !journalID.MatchString(tool.Name) {
			return "", errPrivateProvider
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Name == ordered[i].Name {
			return "", errPrivateProvider
		}
	}
	raw, err := json.Marshal(ordered)
	if err != nil {
		return "", errPrivateProvider
	}
	sum := sha256.Sum256(append([]byte("flywheel:xhs-tool-catalog:v1\n"), raw...))
	return hex.EncodeToString(sum[:]), nil
}
func measuredGuardedSchema(ctx context.Context) (string, error) {
	tools, err := registeredGuardedTools(ctx)
	if err != nil {
		return "", err
	}
	return guardedCatalogDigest(tools)
}
