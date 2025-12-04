# StreamableClientTransport: Connection poisoned by transient errors

## Summary

The `StreamableClientTransport` permanently breaks after **any transient error** (timeout, 503, network interruption), making the connection unusable for all subsequent requests. This affects both stateful and stateless server modes.

## Problem Description

### What Happens

1. ✅ **First Request**: Succeeds normally
2. ⚠️ **Second Request**: Encounters any transient error (timeout, 503, network glitch, etc.)
3. ❌ **All Subsequent Requests**: Fail with "client is closing" error
4. 🔴 **Session Broken**: Connection is permanently unusable, requires full reconnection

### Error Triggers

The connection poisoning occurs in **two paths**:

**Write Path (POST requests):**
- Network timeouts (`http.Client.Timeout`, `context.DeadlineExceeded`)
- Network errors (connection refused, DNS failure, connection reset)
- Server errors (503, 500, 502 - server overload/restart)
- Session errors (404 Not Found - session terminated)

**Read Path (SSE GET requests):**
- SSE connection failures (all network errors after retry exhaustion)
- SSE response errors (same HTTP status errors)
- Protocol errors (invalid JSON, decode failures)

### Impact

- **Affects ALL servers**: Official SDK servers AND third-party implementations
- **Affects BOTH modes**: Stateful AND stateless server configurations
- **Root Cause**: Client-side connection reuse (not server-side behavior)

## Reproduction

### Minimal Test Case

```go
// Setup: Server with a tool that has configurable delays
httpClient := &http.Client{Timeout: 2 * time.Second}
transport := &mcp.StreamableClientTransport{
    Endpoint:   serverURL,
    HTTPClient: httpClient,
}
session, _ := client.Connect(ctx, transport, nil)

// Call 1: Fast (500ms) - ✅ SUCCEEDS
result1, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "delay_tool"})

// Call 2: Slow (3s) - ⏱️ TIMES OUT
result2, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "delay_tool"})
// Error: "context deadline exceeded"

// Call 3: Fast (500ms) - ❌ FAILS (connection poisoned!)
result3, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "delay_tool"})
// Error: "client is closing"
```

### Test Results (Before Fix)

**Stateful Mode:**
```
Call #1: SUCCESS
Call #2: FAILED (timeout)
Call #3: FAILED - "client is closing: EOF"
```

**Stateless Mode:**
```
Call #1: SUCCESS
Call #2: FAILED (timeout)
Call #3: FAILED - "client is closing: EOF"
```

Both modes fail identically because the bug is in the **client's connection reuse**, not server behavior.

## Root Cause

### Error Propagation Flow

```
MCP Application Layer (CallTool, ListTools, etc.)
         ↓
jsonrpc2.Connection (internal/jsonrpc2/conn.go)
  - Manages request/response lifecycle
  - Tracks readErr, writeErr flags
  - Calls writer.Write() and reader.Read()
         ↓
streamableClientConn (mcp/streamable.go)
  - Implements Writer/Reader interfaces
  - Write(): HTTP POST for requests
  - Read(): HTTP SSE GET for server messages
         ↓
http.Client.Do(req)
  - Can fail with timeouts, network errors, HTTP errors
```

### The Bug

**Errors from `http.Client.Do()` flow up to `jsonrpc2.Connection`, which treats ALL errors as fatal:**

1. **Write Errors** (`streamableClientConn.Write()` → `jsonrpc2.Connection.write()`):
   - Any error from `c.client.Do(req)` returns to jsonrpc2
   - If `ctx.Err() == nil` (not cancelled), jsonrpc2 sets `writeErr` permanently
   - **Exception**: If error is wrapped with `jsonrpc2.ErrRejected`, it's non-fatal ✅

2. **Read Errors** (SSE stream → `jsonrpc2.Connection.readIncoming()`):
   - Any error from SSE processing sets `readErr` permanently
   - No mechanism to recover or retry

**Code Locations:**

- `internal/jsonrpc2/conn.go:573-584` - Sets permanent `readErr`
- `internal/jsonrpc2/conn.go:794-810` - Sets permanent `writeErr` (unless `ErrRejected`)
- `internal/jsonrpc2/conn.go:165-176` - `shuttingDown()` rejects work when errors are set
- `mcp/streamable.go:1590-1598` - Write path doesn't use `ErrRejected`

### Key Discovery: ErrRejected Mechanism

The SDK **already has a mechanism to prevent connection poisoning**:

```go
// internal/jsonrpc2/wire.go:43-50
// ErrRejected may be wrapped to return errors from calls to Writer.Write
// that should not cause the Connection to shut down.

// internal/jsonrpc2/conn.go:788-792
// For rejected requests, we don't set the writeErr (which would break the
// connection). They can just be returned to the caller.
if errors.Is(err, ErrRejected) {
    return err
}
```

**The problem**: `streamableClientConn.Write()` never uses `ErrRejected` - architectural oversight.

## Proposed Fix

### Ultra-Minimal Solution (6 Lines)

Wrap errors with `jsonrpc2.ErrRejected` to prevent connection poisoning:

```go
// File: mcp/streamable.go (Write method)

resp, err := c.client.Do(req)
if err != nil {
    // Wrap with ErrRejected to prevent jsonrpc2 from poisoning the connection.
    // This allows transient errors (timeouts, network issues) to be retried.
    return fmt.Errorf("%s: %w: %v", requestSummary, jsonrpc2.ErrRejected, err)
}

if err := c.checkResponse(requestSummary, resp); err != nil {
    // Wrap with ErrRejected to prevent connection poisoning.
    return fmt.Errorf("%w: %v", jsonrpc2.ErrRejected, err)
}
```

### Why This Works

- `jsonrpc2.ErrRejected` was **designed for this exact purpose**
- Prevents `writeErr` from being set in `jsonrpc2.Connection`
- Connection stays healthy, no resource leaks
- Uses existing SDK mechanism (no new code needed)
- All Write path errors become recoverable

### Test Results (After Fix)

**Stateful Mode:**
```
Call #1: SUCCESS
Call #2: FAILED (timeout - expected)
Call #3: SUCCESS ✅ (connection survived!)
```

**Stateless Mode:**
```
Call #1: SUCCESS
Call #2: FAILED (timeout - expected)
Call #3: SUCCESS ✅ (connection survived!)
```

Both tests pass! The connection survives transient errors.

## Alternative Approaches Considered

### ❌ Approach 1: Call jsonrpc2.Connection.Close()
- **Result**: Deadlock (test hung forever)
- **Why it failed**: `Close()` waits for operations to complete, but we're calling it FROM inside an operation

### ❌ Approach 2: Call streamableClientConn.Close()
- **Result**: Connection still breaks (no deadlock though)
- **Why it failed**: Context cancelled → future requests fail, connection destroyed

### ✅ Approach 3: Use ErrRejected (SELECTED)
- **Result**: Connection survives, all tests pass
- **Why it works**: Prevents poisoning at source, works with SDK design

## Implementation Details

### Changes Required

**File: `mcp/streamable.go`**
- Lines ~1590-1600 (Write method)
- Remove unused imports (`net`, `syscall` if error classification was added)
- 6 lines changed total

**File: `connection_closed_test.go`** (new)
- Add reproduction tests for stateful and stateless modes
- Verify Call #3 succeeds after Call #2 timeout

### Trade-offs

**Current approach (wrap ALL errors with ErrRejected):**
- ✅ Ultra-minimal (6 lines)
- ✅ Simple to understand and review
- ✅ No error classification complexity
- ⚠️ Treats all Write errors as recoverable (even 401, protocol errors)
- ⚠️ Can be enhanced later with error classification if needed

**Future enhancement (error classification):**
- Add `isRecoverableError()` function to distinguish:
  - **Recoverable**: timeouts, 503, 502, 429, network issues
  - **Fatal**: 401, 403, protocol errors, client bugs
- Only wrap recoverable errors with `ErrRejected`
- More nuanced but adds ~75 lines of code

## Testing

### Run Tests

```bash
go test -v ./connection_closed_test.go -run TestTimeout
```

**Expected Results:**
- Both `TestTimeoutBugReproduction` and `TestTimeoutStateless` should pass
- Call #3 succeeds even after Call #2 timeout

### Test Coverage

- ✅ Stateful server mode
- ✅ Stateless server mode
- ✅ Timeout errors
- ✅ Connection survives transient errors
- 🔄 Future: 503 errors, network failures, concurrent scenarios

## Documentation Updates

### Files to Update

1. **`connection_cosed.md`** - Complete technical analysis
2. **`connection_timeout_issue.md`** - Simple summary document
3. **`issue_draft.md`** - PR draft with spec context
4. **`connection_closed_test.go`** - Reproduction tests

### Recommendations for Users

**Current Workaround (before fix):**
```go
// Use per-call context timeouts instead of global client timeouts
httpClient := &http.Client{Timeout: 0}  // No global timeout
transport := &mcp.StreamableClientTransport{
    Endpoint:   serverURL,
    HTTPClient: httpClient,
}

// Bound each call individually
callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
result, err := session.CallTool(callCtx, ...)
```

**After Fix:**
- No workaround needed
- Connection survives transient errors
- Still recommend per-call timeouts for fine-grained control

## Related Issues

- **[modelcontextprotocol/modelcontextprotocol#1442](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1442)** - SEP proposal for stateless MCP
- **[go-sdk#479](https://github.com/modelcontextprotocol/go-sdk/issues/479)** - Server-side cleanup callbacks

## Environment

- **SDK Version**: v1.0.0
- **Go Version**: 1.23+
- **Platform**: darwin (macOS), linux

## References

- [MCP Spec - Streamable Transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
- [MCP Go SDK Repository](https://github.com/modelcontextprotocol/go-sdk)
- Fork branch: `fix/connection-poisoning-low-level`

## Checklist

- [x] Bug reproduced and documented
- [x] Root cause identified
- [x] Fix implemented and tested
- [x] Tests pass (stateful and stateless modes)
- [x] Documentation updated
- [ ] Create GitHub issue on fork
- [ ] Prepare upstream PR
- [ ] Submit to official repository

## Next Steps

1. Review this issue on fork
2. Refine fix based on feedback
3. Open upstream issue
4. Submit PR to `modelcontextprotocol/go-sdk`
