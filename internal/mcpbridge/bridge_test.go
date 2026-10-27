package mcpbridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chux0519/memspace/internal/mcpbridge"
)

type echoArgs struct {
	Text string `json:"text"`
}

// remoteServer mimics MemSpace's /mcp: stateless, JSON responses, bearer
// token required.
func remoteServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "memspace", Version: "test"},
			&mcp.ServerOptions{Instructions: "use memspace_echo"})
		mcp.AddTool(s, &mcp.Tool{Name: "memspace_echo", Description: "echo"},
			func(ctx context.Context, req *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}}}, nil, nil
			})
		mcp.AddTool(s, &mcp.Tool{Name: "memspace_ping", Description: "no arguments"},
			func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
			})
		return s
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// agent connects an MCP client to a bridge running on in-memory transports.
func agent(t *testing.T, ctx context.Context, opts mcpbridge.Options) *mcp.ClientSession {
	t.Helper()
	agentSide, bridgeSide := mcp.NewInMemoryTransports()
	errc := make(chan error, 1)
	go func() { errc <- mcpbridge.Run(ctx, opts, bridgeSide) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent"}, nil).Connect(ctx, agentSide, nil)
	if err != nil {
		select {
		case err := <-errc:
			t.Fatalf("bridge: %v", err)
		default:
		}
		t.Fatalf("connect to bridge: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content: %+v", res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	return tc.Text
}

func TestBridgeForwardsTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := remoteServer(t, "msm_secret")
	cs := agent(t, ctx, mcpbridge.Options{URL: srv.URL, Token: "msm_secret", Version: "test"})

	if got := cs.InitializeResult().Instructions; got != "use memspace_echo" {
		t.Errorf("instructions = %q", got)
	}
	var names []string
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "memspace_echo,memspace_ping" {
		t.Fatalf("tools = %v", names)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "memspace_echo", Arguments: map[string]any{"text": "幂等 去重"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "echo: 幂等 去重" {
		t.Errorf("echo = %q", got)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "memspace_ping"})
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, res); got != "pong" {
		t.Errorf("ping = %q", got)
	}

	// The server goes away mid-session: the call fails as a tool error the
	// model can read, not as a broken MCP connection.
	srv.Close()
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "memspace_ping"})
	if err != nil {
		t.Fatalf("protocol error after server stopped: %v", err)
	}
	if !res.IsError || !strings.Contains(text(t, res), "unreachable") {
		t.Errorf("result after server stopped = %+v", res)
	}
}

func TestBridgeRejectsBadToken(t *testing.T) {
	srv := remoteServer(t, "msm_secret")
	_, err := mcpbridge.Connect(context.Background(), mcpbridge.Options{URL: srv.URL, Token: "msm_wrong"})
	if err == nil {
		t.Fatal("connected with a wrong token")
	}
}
