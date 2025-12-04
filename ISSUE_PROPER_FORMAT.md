## Describe the bug

The `StreamableClientTransport` permanently breaks after any transient error (timeout, 503, network interruption), making the connection unusable for all subsequent requests. This affects both stateful and stateless server modes.

After a single transient error (such as a timeout), all future requests fail with "client is closing" errors, even though the error was temporary and should be recoverable.

## To Reproduce

### Minimal reproduction code

```go
package main

import (
    "context"
    "net/http"
    "time"
    "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
    ctx := context.Background()

    // Configure client with global timeout
    httpClient := &http.Client{Timeout: 2 * time.Second}
    transport := &mcp.StreamableClientTransport{
        Endpoint:   "http://localhost:8080",
        HTTPClient: httpClient,
    }

    client := mcp.NewClient(&mcp.Implementation{
        Name:    "test-client",
        Version: "1.0.0",
    }, nil)

    session, _ := client.Connect(ctx, transport, nil)

    // Call 1: Fast operation (500ms) - ✅ SUCCEEDS
    result1, err1 := session.CallTool(ctx, &mcp.CallToolParams{
        Name: "fast_tool",
    })
    // Success

    // Call 2: Slow operation (3s) - ⏱️ TIMES OUT
    result2, err2 := session.CallTool(ctx, &mcp.CallToolParams{
        Name: "slow_tool",
    })
    // Error: "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"

    // Call 3: Fast operation (500ms) - ❌ SHOULD SUCCEED BUT FAILS
    result3, err3 := session.CallTool(ctx, &mcp.CallToolParams{
        Name: "fast_tool",
    })
    // Error: "client is closing"
    // Connection is now permanently poisoned!
}
```

### Steps to reproduce

1. Create an MCP server with a tool that has configurable delays
2. Connect using `StreamableClientTransport` with `http.Client{Timeout: 2 * time.Second}`
3. Call a tool that completes quickly (< 2s) - succeeds
4. Call a tool that takes longer than the timeout (> 2s) - times out as expected
5. Call the fast tool again - **fails with "client is closing"**

All subsequent requests fail permanently until reconnection.

## What I saw

```
Call #1: SUCCESS
Call #2: FAILED - "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
Call #3: FAILED - "client is closing: EOF"
Call #4: FAILED - "client is closing: EOF"
...all future calls fail...
```

## What I expected to see

```
Call #1: SUCCESS
Call #2: FAILED - "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
Call #3: SUCCESS  ✅ (connection should survive the transient timeout)
Call #4: SUCCESS  ✅
...connection continues working...
```

The connection should remain healthy after transient errors (timeouts, 503, network hiccups), only breaking on fatal errors (401 authentication failure, protocol errors, etc.).

## Root cause analysis

The bug occurs in the **Write path** (POST requests for CallTool, ListTools, etc.): `streamableClientConn.Write()` doesn't wrap transient errors with `jsonrpc2.ErrRejected`, causing the `jsonrpc2.Connection` to permanently set its `writeErr` flag.

**Error flow:**

1. `http.Client.Do(req)` encounters a timeout during a POST request (CallTool, ListTools, etc.)
2. Error returns to `streamableClientConn.Write()` (mcp/streamable.go:1590)
3. Error propagates to `jsonrpc2.Connection.write()` (internal/jsonrpc2/conn.go:781-810)
4. Since error is NOT wrapped with `ErrRejected`, jsonrpc2 sets `writeErr` permanently
5. `shuttingDown()` check (conn.go:165-176) blocks all future work
6. Connection enters "zombie state" - open but unusable for **all MCP operations**

**The SDK already has the fix mechanism:**

```go
// internal/jsonrpc2/wire.go:43-50
// ErrRejected may be wrapped to return errors from calls to Writer.Write
// that should not cause the Connection to shut down.

// internal/jsonrpc2/conn.go:788-792
if errors.Is(err, ErrRejected) {
    return err  // Don't set writeErr
}
```

The mechanism exists but `streamableClientConn.Write()` never uses it.

**Note**: This fix addresses the Write path. The Read path (SSE GET requests) can also set `readErr`, but Write path failures are more common in practice since every CallTool/ListTools/etc. uses the Write path.

## Proposed fix

### Philosophical Insight: HTTP is Stateless

For **Streamable HTTP transport**, the underlying protocol is HTTP, which is stateless. Unlike true bidirectional streams (stdin/stdio, WebSockets), HTTP-based transports have no persistent connection that can "break":

- Each POST request is independent
- SSE GET failures don't affect POST requests
- Connection "poisoning" is a client-side abstraction, not a transport failure

**Key observation from server-side code:**
```go
// mcp/streamable.go:1177-1182 (SERVER side)
// Most errors don't break the connection: unlike a true bidirectional
// stream, a failure to deliver to a stream is not an indication that the
// logical session is broken.
```

The server already implements this philosophy - the client should too.

### The Fix: Never Poison Connection for HTTP Transport

**Write Path (6 lines):**
Wrap ALL HTTP errors with `jsonrpc2.ErrRejected` to prevent `writeErr` from being set:

```go
// In streamableClientConn.Write() method
resp, err := c.client.Do(req)
if err != nil {
    return fmt.Errorf("%s: %w: %v", requestSummary, jsonrpc2.ErrRejected, err)
}

if err := c.checkResponse(requestSummary, resp); err != nil {
    return fmt.Errorf("%w: %v", jsonrpc2.ErrRejected, err)
}
```

**Read Path (SSE stream):**
Remove ALL `c.fail()` calls that poison the connection on SSE errors:

```go
// In handleSSE, connectStandaloneSSE, handleJSON, processStream:
// OLD: c.fail(err)  // ❌ Poisons connection
// NEW: return       // ✅ Just exit, don't poison

// For decode errors in SSE stream:
// OLD: c.fail(err); return "", 0, true
// NEW: continue  // ✅ Skip bad event, continue processing
```

**Result**: Connection survives ALL transient errors (timeouts, 503, network issues, SSE failures, decode errors). Only explicit user calls to `Close()` terminate the connection.

## Test results

I've implemented and tested this fix in branch `fix/connection-poisoning-low-level`:

**Before fix:**
```
TestTimeoutBugReproduction (stateful):
  Call #1: SUCCESS
  Call #2: FAILED (timeout)
  Call #3: FAILED - "client is closing: EOF"
--- FAIL

TestTimeoutStateless (stateless):
  Call #1: SUCCESS
  Call #2: FAILED (timeout)
  Call #3: FAILED - "client is closing: EOF"
--- FAIL
```

**After fix:**
```
TestTimeoutBugReproduction (stateful):
  Call #1: SUCCESS
  Call #2: FAILED (timeout)
  Call #3: SUCCESS ✅
--- PASS

TestTimeoutStateless (stateless):
  Call #1: SUCCESS
  Call #2: FAILED (timeout)
  Call #3: SUCCESS ✅
--- PASS
```

## Version information

- **Go MCP SDK version:** v1.0.0
- **Go version:** `go version go1.23.3 darwin/arm64`
- **Platform:** macOS (also affects Linux, Windows)

## Additional context

### Affects both stateful and stateless modes

The bug is **client-side**, not server-side. Even in stateless mode, the client reuses the same `jsonrpc2.Connection` across requests, so the bug manifests identically in both modes.

### Error triggers (comprehensive list)

**Write Path (POST requests):**
- Network timeouts (http.Client.Timeout, context.DeadlineExceeded)
- Network errors (connection refused, DNS failure, connection reset)
- Server errors (503, 500, 502)
- Session errors (404 Not Found)

**Read Path (SSE GET requests):**
- SSE connection failures
- SSE response errors
- Protocol errors (invalid JSON, decode failures)

### Why this is critical

After any transient error, the connection becomes completely unusable for **all MCP operations**:
- ❌ `CallTool()` fails with "client is closing"
- ❌ `ListTools()` fails with "client is closing"
- ❌ `ListResources()` fails with "client is closing"
- ❌ All other MCP methods fail

The entire MCP session must be discarded and recreated, even though the error was temporary and should have been recoverable. This severely impacts reliability in production environments where transient errors (timeouts, 503 during deployments, network hiccups) are expected and normal.

### Related issues

- #479 - Server-side cleanup callbacks for closed connections
- Related to stateless discussion in modelcontextprotocol/modelcontextprotocol#1442

### Implementation branch

I have a working fix with tests on fork: https://github.com/Agustin-Jerusalinsky/go-sdk/tree/fix/connection-poisoning-low-level

The fix includes:
- 6-line change to `mcp/streamable.go`
- Reproduction tests for both stateful and stateless modes
- Comprehensive documentation of root cause and alternatives explored

I'm happy to submit a PR if this approach looks good!
