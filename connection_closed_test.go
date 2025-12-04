// Package gosdk provides tests to reproduce the MCP SDK connection poisoning bug.
// After any transient error (timeout, 503, network issue), the connection becomes
// permanently unusable with "client is closing" errors.
package gosdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testDelays defines the sleep duration for each call
var testDelays = []time.Duration{
	500 * time.Millisecond, // Call 1: fast, should succeed
	3 * time.Second,        // Call 2: slow, will timeout
	500 * time.Millisecond, // Call 3: fast, should succeed but fails due to bug
}

// createDelayTool creates an MCP tool that sleeps for configurable durations
func createDelayTool(callCount *atomic.Int32) (*mcp.Server, *mcp.Tool) {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	tool := &mcp.Tool{
		Name:        "delay_tool",
		Description: "Tool with configurable delays for testing",
	}

	handler := func(ctx context.Context, req *mcp.CallToolRequest, args any) (*mcp.CallToolResult, any, error) {
		callNum := int(callCount.Add(1))
		delay := testDelays[0] // default
		if callNum <= len(testDelays) {
			delay = testDelays[callNum-1]
		}

		time.Sleep(delay)

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Call #%d completed", callNum),
				},
			},
		}, nil, nil
	}

	mcp.AddTool(server, tool, handler)
	return server, tool
}

// setupTestServer creates and starts an HTTP test server with MCP handler
func setupTestServer(t *testing.T, server *mcp.Server, stateless bool) *httptest.Server {
	var opts *mcp.StreamableHTTPOptions
	if stateless {
		opts = &mcp.StreamableHTTPOptions{Stateless: true}
	}

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, opts)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

// createMCPSession creates and connects an MCP client session
func createMCPSession(t *testing.T, serverURL string, clientTimeout time.Duration) *mcp.ClientSession {
	ctx := context.Background()
	httpClient := &http.Client{Timeout: clientTimeout}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   serverURL,
		HTTPClient: httpClient,
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return session
}

// callResult represents the result of a CallTool invocation
type callResult struct {
	num    int
	err    error
	result *mcp.CallToolResult
}

// performCallSequence executes three sequential tool calls and returns results
func performCallSequence(session *mcp.ClientSession) []callResult {
	results := make([]callResult, 3)

	for i := range 3 {
		ctx := context.Background() // Fresh context for each call
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "delay_tool",
			Arguments: map[string]any{},
		})
		results[i] = callResult{num: i + 1, err: err, result: result}
	}

	return results
}

// reportResults logs the test results
func reportResults(t *testing.T, results []callResult) {
	t.Helper()

	for _, r := range results {
		if r.err != nil {
			t.Logf("Call #%d: FAILED - %v", r.num, r.err)
		} else {
			t.Logf("Call #%d: SUCCESS", r.num)
		}
	}

	// Expected behavior: Call 3 should succeed even after Call 2 timeout
	if results[2].err != nil {
		t.Errorf("Call #3 failed after transient error in Call #2")
	}
}

// TestTimeoutBugReproduction tests connection behavior after timeout in stateful mode.
// Expected: Call #3 should succeed even after Call #2 timeout.
func TestTimeoutBugReproduction(t *testing.T) {
	var callCount atomic.Int32
	server, _ := createDelayTool(&callCount)
	httpServer := setupTestServer(t, server, false) // stateful mode
	session := createMCPSession(t, httpServer.URL, 2*time.Second)

	results := performCallSequence(session)
	reportResults(t, results)
}

// TestTimeoutStateless tests connection behavior after timeout in stateless mode.
// Expected: Call #3 should succeed even after Call #2 timeout.
func TestTimeoutStateless(t *testing.T) {
	var callCount atomic.Int32
	server, _ := createDelayTool(&callCount)
	httpServer := setupTestServer(t, server, true) // stateless mode
	session := createMCPSession(t, httpServer.URL, 2*time.Second)

	results := performCallSequence(session)
	reportResults(t, results)
}
