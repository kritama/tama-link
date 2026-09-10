package server

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerExposesOnlySubmitAndAwait(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := New("test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "tama-link-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)

	want := []string{"await", "submit"}
	if !slices.Equal(names, want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
}
