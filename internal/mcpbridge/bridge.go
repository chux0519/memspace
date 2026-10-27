// Package mcpbridge serves MemSpace's MCP tools to a local agent over stdio,
// forwarding every call to the server's streamable HTTP endpoint.
//
// Agents then only need a command in their configuration
// (`memspace mcp stdio`): the bridge reads the device's credentials itself,
// so no token is written into agent configs or the shell environment, and
// a rotated token or a moved server needs no change on the agent side.
//
// Both legs use the MCP SDK, so each negotiates its own protocol version and
// headers; the bridge mirrors the server's tools and instructions as they
// are when it starts.
package mcpbridge

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configure a bridge.
type Options struct {
	// URL is the server's MCP endpoint, e.g. http://host:8765/mcp.
	URL string
	// Token is sent as a bearer token (the device's read-only MCP token).
	Token string
	// Transport carries the HTTP requests (e.g. one that skips proxies for
	// private hosts). Nil means http.DefaultTransport.
	Transport http.RoundTripper
	// Version is reported to both sides.
	Version string
}

// ConnectTimeout bounds reaching the server at start-up. Agents give MCP
// servers only a few seconds to start.
const ConnectTimeout = 8 * time.Second

// Run connects to the server, then serves its tools on t until the agent
// disconnects or ctx is cancelled.
func Run(ctx context.Context, opts Options, t mcp.Transport) error {
	remote, err := Connect(ctx, opts)
	if err != nil {
		return err
	}
	defer remote.Close()
	srv, err := Mirror(ctx, remote, opts.Version)
	if err != nil {
		return err
	}
	return srv.Run(ctx, t)
}

// Connect opens a client session to the server's MCP endpoint.
func Connect(ctx context.Context, opts Options) (*mcp.ClientSession, error) {
	base := opts.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:   opts.URL,
		HTTPClient: &http.Client{Transport: bearer{token: opts.Token, base: base}, Timeout: 60 * time.Second},
		// The server is stateless and answers with JSON; there is no stream
		// for server-initiated messages.
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "memspace-bridge", Version: opts.Version}, nil)
	cctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	cs, err := client.Connect(cctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", opts.URL, err)
	}
	return cs, nil
}

// Mirror returns a server that offers remote's tools and instructions and
// forwards each tool call to remote.
func Mirror(ctx context.Context, remote *mcp.ClientSession, version string) (*mcp.Server, error) {
	impl := &mcp.Implementation{Name: "memspace", Version: version}
	var instructions string
	if init := remote.InitializeResult(); init != nil {
		instructions = init.Instructions
		if init.ServerInfo != nil {
			impl = &mcp.Implementation{Name: init.ServerInfo.Name, Title: init.ServerInfo.Title, Version: init.ServerInfo.Version}
		}
	}
	srv := mcp.NewServer(impl, &mcp.ServerOptions{Instructions: instructions})
	lctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	for tool, err := range remote.Tools(lctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		srv.AddTool(tool, forward(remote, tool.Name))
	}
	return srv, nil
}

// forward calls the tool on the server. Transport failures become tool
// errors, which agents show to the model, rather than protocol errors.
func forward(remote *mcp.ClientSession, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params := &mcp.CallToolParams{Name: name}
		if len(req.Params.Arguments) > 0 {
			params.Arguments = req.Params.Arguments
		}
		res, err := remote.CallTool(ctx, params)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "MemSpace server unreachable: " + err.Error()}},
			}, nil
		}
		return res, nil
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
