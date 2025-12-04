# MCP Go SDK: Connection Poisoning on Transient Errors

> **Note**: This analysis is based on a fork of the [official MCP Go SDK repository](https://github.com/modelcontextprotocol/go-sdk) for investigation and potential PR submission.

## Problem Summary

The MCP Go SDK has a critical bug where **any transient error permanently breaks the client connection**, making it unusable for all subsequent requests.

### Error Triggers (Comprehensive)

The connection poisoning occurs in **two paths** (see `connection_cosed.md` for full details):

**Write Path** (POST requests):
- ⏱️ **Network timeouts** (http.Client.Timeout, context.DeadlineExceeded)
- 🔌 **Network errors** (connection refused, DNS failure, connection reset)
- 🔴 **Server errors** (503, 500, 502 - server overload/restart)
- 🚫 **Session errors** (404 Not Found - session terminated)

**Read Path** (SSE GET requests):
- 📡 **SSE connection failures** (all network errors after retry exhaustion)
- 🔴 **SSE response errors** (same HTTP status errors)
- 📝 **Protocol errors** (invalid JSON, decode failures)

> **See:** `connection_cosed.md` sections "Error Triggers - Complete List" and "Error Classification" for detailed analysis of each error type and which should be recoverable vs fatal.

## What Happens

1. ✅ **First Request**: Succeeds normally
2. ⚠️ **Second Request**: Encounters any transient error (timeout, 503, network glitch, etc.)
3. ❌ **All Subsequent Requests**: Fail with "client is closing" error
4. 🔴 **Session Broken**: Connection is permanently poisoned, requires full reconnection

## Root Cause

The issue occurs because:

- **All Errors Treated as Fatal**: The SDK treats any read or write error as terminal:
  - Regular POST requests (tool calls) - any HTTP error sets `writeErr`
  - Long-lived SSE GET connections (server messages) - any interruption sets `readErr`
  - No distinction between transient (503, timeout) vs permanent (auth failure) errors

- **Permanent Error State**: When any error occurs, the SDK's internal `jsonrpc2.Connection` sets `readErr` or `writeErr` flags that **never get cleared**

- **All Future Work Rejected**: Once these flags are set, the connection rejects all new requests via the `shuttingDown()` check

- **Common Trigger**: Setting `http.Client.Timeout > 0` makes this worse by applying a global timeout to both short requests AND the long-lived SSE connection

## Why This is Critical for MCP

MCP's Streamable HTTP transport uses **Server-Sent Events (SSE)** which maintain a **long-lived connection** that can be interrupted by:
- Server restarts or deployments (503 errors)
- Load balancer timeouts
- Network hiccups
- Global HTTP client timeouts

Any of these transient issues permanently poison the connection, requiring full reconnection instead of graceful recovery.

## Affects Both Stateful and Stateless Modes

**Important**: This bug affects **BOTH** server modes:

- **Stateful Mode**: Connection poisoning occurs after transient errors
- **Stateless Mode**: Also affected despite creating "temporary" sessions per request

**Why stateless mode doesn't help**: The bug is in the **client SDK**, not the server. Even though the server may treat requests as stateless, the **client still reuses the same `jsonrpc2.Connection`** across requests. When that connection is poisoned by `readErr`/`writeErr`, all subsequent requests fail regardless of server mode.

**Test Confirmation**: Both `TestTimeoutBugReproduction` (stateful) and `TestTimeoutStateless` (stateless) demonstrate identical failures after transient errors.

## Code Location

**SDK Files Affected:**
- `internal/jsonrpc2/conn.go:560-570` - Sets permanent `readErr`
- `internal/jsonrpc2/conn.go:781-799` - Sets permanent `writeErr`
- `internal/jsonrpc2/conn.go:166-176` - Rejects work when errors are set
- `mcp/streamable.go:1559-1570` - POST requests
- `mcp/streamable.go:1828-1843` - SSE GET requests

## Reproduction

```go
// ❌ WRONG: Global timeout breaks the connection
httpClient := &http.Client{
    Timeout: 2 * time.Second,  // This will poison the connection
}
transport := &mcp.StreamableClientTransport{
    Endpoint:   serverURL,
    HTTPClient: httpClient,
}

session, _ := client.Connect(ctx, transport, nil)
result1, _ := session.CallTool(ctx, ...)  // ✅ Succeeds
result2, _ := session.CallTool(ctx, ...)  // ⏱️ Times out (slow server)
result3, _ := session.CallTool(ctx, ...)  // ❌ Fails: "client is closing"
```

## Solution

**Use per-call context timeouts instead of global client timeouts:**

```go
// ✅ CORRECT: No global timeout, use per-call context deadlines
httpClient := &http.Client{
    Timeout: 0,  // No global timeout for streamable transport
}
transport := &mcp.StreamableClientTransport{
    Endpoint:   serverURL,
    HTTPClient: httpClient,
}

session, _ := client.Connect(ctx, transport, nil)

// Bound each call individually
callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
result, err := session.CallTool(callCtx, ...)
```

## Proposed Fix (Upstream)

**PR Draft**: See `issue_draft.md`

1. **Guardrail**: Reject `http.Client` with `Timeout > 0` in `StreamableClientTransport.Connect()`
2. **Graceful Error Handling**: Treat transient errors (timeouts, 503, network errors) as "rejected" writes (non-fatal) using `jsonrpc2.ErrRejected`
3. **Error Classification**: Distinguish between recoverable and terminal errors
4. **Documentation**: Update examples to show proper usage with per-call timeouts
5. **Tests**: Add recovery tests for both stateful and stateless modes with various error types

## Related Issues

- **[modelcontextprotocol/modelcontextprotocol#1442](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1442)** - SEP proposal to make MCP stateless by default
- **[go-sdk#479](https://github.com/modelcontextprotocol/go-sdk/issues/479)** - Server-side cleanup callbacks for closed connections

## Current Status

- **SDK Version**: v1.0.0
- **Bug Status**: Not yet reported upstream
- **Impact**: Affects all transient errors (timeouts, 503, network issues, etc.)
- **Workaround**: Use `http.Client{Timeout: 0}` + per-call context deadlines + client-side reconnection logic
- **Fix Status**: Draft PR in progress

## Testing

Run reproduction tests:
```bash
go test -v ./connection_closed_test.go -run TestTimeout
```

**Expected behavior**: Call #3 should succeed even after Call #2 timeout
**Actual behavior**: Call #3 fails with "client is closing" error in both stateful and stateless modes

## References

- [MCP Spec - Streamable Transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
- Detailed analysis: `connection_cosed.md`
- PR draft: `issue_draft.md`
- Test case: `connection_closed_test.go`
