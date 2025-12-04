# MCP Go SDK Connection Timeout Issue

**Status:** Known Issue - Workaround Implemented
**Created:** 2025-12-02
**Updated:** 2025-12-03
**Severity:** Critical - Causes permanent session failure after timeout

---

## Problem Description

The official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk@v1.0.0`) has a critical bug where **any transient error permanently breaks the client connection**, making the session unusable for all subsequent requests.

### What Happens

1. **Normal Operation:** Client establishes connection to MCP server
2. **Transient Error Occurs:** A single request encounters ANY of the following errors:
   - Network timeout (http.Client.Timeout, context.DeadlineExceeded)
   - Network error (connection refused, DNS failure, connection reset)
   - Server error (503 Service Unavailable, 500 Internal Server Error, etc.)
   - Session error (404 Not Found - session terminated)
   - Protocol error (invalid JSON, SSE decode failure)
3. **Connection Broken:** The underlying `jsonrpc2.Connection` sets `readErr` or `writeErr` internally
4. **Permanent Failure:** ALL subsequent requests fail with "client is closing" error
5. **Session Unusable:** The session cannot recover and must be replaced

### Error Triggers - Complete List

The connection poisoning is triggered by errors in **two paths**:

#### Write Path (POST requests - tool calls, notifications)
**Location:** `mcp/streamable.go:1560-1648 (Write method)`

1. **Network/Transport Errors** (line 1590-1593):
   ```go
   resp, err := c.client.Do(req)
   if err != nil {
       return fmt.Errorf("%s: %v", requestSummary, err)
   }
   ```
   Triggers:
   - HTTP client timeout (`http.Client.Timeout > 0`)
   - Context deadline exceeded (`context.WithTimeout`)
   - Connection refused (server down)
   - DNS resolution failure
   - TLS handshake failure
   - Connection reset by peer
   - Network unreachable
   - Any `net.Error` from the HTTP transport

2. **HTTP Status Errors** (line 1595-1598, calls checkResponse):
   ```go
   if err := c.checkResponse(requestSummary, resp); err != nil {
       c.fail(err)  // Also marks connection as failed locally
       return err
   }
   ```
   Triggers (line 1740-1758):
   - 404 Not Found (session terminated by server)
   - 503 Service Unavailable (server overloaded/restarting)
   - 500 Internal Server Error
   - 502 Bad Gateway
   - Any non-2xx status code

#### Read Path (SSE GET requests - server notifications)
**Location:** `mcp/streamable.go:1694-1734 (handleSSE method)`

1. **SSE Connection Failures** (line 1717-1724):
   ```go
   newResp, err := c.connectSSE(ctx, lastEventID, reconnectDelay, false)
   if err != nil {
       if ctx.Err() == nil {
           c.fail(fmt.Errorf("%s: failed to reconnect: %v", requestSummary, err))
       }
       return
   }
   ```
   Triggers (after all reconnection attempts fail):
   - Same network/transport errors as Write path
   - Long-lived SSE GET interrupted by `http.Client.Timeout`
   - Load balancer timeout on SSE connection
   - Server restart/deployment breaking SSE

2. **SSE Response Errors** (line 1729-1732):
   ```go
   if err := c.checkResponse(requestSummary, resp); err != nil {
       c.fail(err)
       return
   }
   ```
   Triggers:
   - Same HTTP status errors as Write path

3. **Protocol/Decode Errors** (line 1790 in processStream):
   ```go
   msg, err := jsonrpc.DecodeMessage(evt.Data)
   if err != nil {
       c.fail(fmt.Errorf("%s: failed to decode event: %v", requestSummary, err))
       return
   }
   ```
   Triggers:
   - Invalid JSON in SSE event data
   - Malformed JSON-RPC message
   - Encoding issues

### Impact

**Affects ALL MCP servers using StreamableClientTransport:**
- ❌ **All Servers:** Official SDK servers AND mark3labs servers
- ❌ **Both Modes:** Stateful AND stateless server configurations
- ❌ **Root Cause:** Client-side connection reuse (not server-side behavior)

**User Experience:**
- Conversations fail after first timeout on ANY MCP server
- All MCP tools become unavailable until reconnection
- **Workaround Implemented:** Automatic reconnection in Maxwell client layer

---

## Root Cause Analysis

### Why ALL Servers Are Affected

**Critical Discovery:** The bug is in the **client SDK**, not the server. The `StreamableClientTransport` **always maintains a persistent connection** regardless of server mode.

#### Client Connection Persistence

**File:** `github.com/modelcontextprotocol/go-sdk@v1.0.0/mcp/streamable.go:1031-1076`

```go
// StreamableClientTransport.Connect() creates a persistent connection
func (t *StreamableClientTransport) Connect(ctx context.Context) (Connection, error) {
	// ...
	connCtx, cancel := context.WithCancel(ctx)
	conn := &streamableClientConn{
		url:        t.Endpoint,
		client:     client,
		incoming:   make(chan jsonrpc.Message, 10),  // ← Reusable channel
		done:       make(chan struct{}),
		ctx:        connCtx,                          // ← Persistent context
		cancel:     cancel,
		sessionID:  "",                               // ← Maintained across requests
		// ...
	}
	return conn, nil
}

type streamableClientConn struct {
	url               string
	client            *http.Client
	ctx               context.Context    // ← Lives for entire session
	cancel            context.CancelFunc
	incoming          chan jsonrpc.Message // ← Reused for all requests
	sessionID         string               // ← Persists across requests
	initializedResult *InitializeResult    // ← Cached
	// ...
}
```

**The Problem:** This `streamableClientConn` wraps a `jsonrpc2.Connection` that is **reused for all requests**. When the underlying connection breaks, all future requests fail.

### Error Propagation Flow

Understanding how errors flow from HTTP layer to jsonrpc2 layer:

```
┌─────────────────────────────────────────────────────────────┐
│ MCP Application Layer (CallTool, ListTools, etc.)         │
└────────────────────┬────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│ jsonrpc2.Connection (internal/jsonrpc2/conn.go)           │
│ - Manages request/response lifecycle                        │
│ - Tracks readErr, writeErr flags                           │
│ - Calls writer.Write() and reader.Read()                   │
└────────────────────┬────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│ streamableClientConn (mcp/streamable.go)                   │
│ Implements Writer/Reader interfaces:                        │
│ - Write(): HTTP POST for requests                          │
│ - Read(): HTTP SSE GET for server messages                 │
└────────────────────┬────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│ http.Client.Do(req)                                        │
│ Can fail with:                                              │
│ - net.Error (timeouts, connection errors)                  │
│ - HTTP status errors (5xx, 4xx)                            │
│ - Protocol errors (invalid responses)                       │
└─────────────────────────────────────────────────────────────┘
```

**Key Problem:** Errors from `http.Client.Do()` flow up to `jsonrpc2.Connection`, which treats **ALL** errors as fatal:

1. **Write Errors** (`streamableClientConn.Write()` → `jsonrpc2.Connection.write()`):
   - Any error from `c.client.Do(req)` returns to jsonrpc2
   - If `ctx.Err() == nil` (not cancelled), jsonrpc2 sets `writeErr` permanently
   - Exception: If error is wrapped with `jsonrpc2.ErrRejected`, it's non-fatal

2. **Read Errors** (SSE stream → `jsonrpc2.Connection.readIncoming()`):
   - Any error from SSE processing sets `readErr` permanently
   - No mechanism to recover or retry

**Why This Design Fails:**
- Cannot distinguish transient errors (503, timeout) from permanent errors (401, TLS failure)
- No error classification or retry logic
- Once `readErr` or `writeErr` is set, connection refuses all future work

### SDK JSONRPC2 Bug

The bug exists in the internal JSONRPC2 connection handler:

**File:** `github.com/modelcontextprotocol/go-sdk@v1.0.0/internal/jsonrpc2/conn.go`

#### 1. Read Loop Sets Error Permanently (Lines 532-571)

```go
func (c *Connection) readIncoming(ctx context.Context, reader Reader, preempter Preempter) {
	var err error
	for {
		var msg Message
		msg, err = reader.Read(ctx)  // ⏱️ TIMEOUT HAPPENS HERE
		if err != nil {
			break  // 🛑 Exits loop
		}
		// ... process message
	}

	c.updateInFlight(func(s *inFlightState) {
		s.reading = false
		s.readErr = err  // ❌ SETS ERROR PERMANENTLY (never cleared!)

		// Retire any outgoing requests that were still in flight
		for id, ac := range s.outgoingCalls {
			ac.retire(&Response{ID: id, Error: err})
		}
		s.outgoingCalls = nil
	})
}
```

#### 2. Shutdown Check Blocks Future Calls (Lines 165-183)

```go
func (s *inFlightState) shuttingDown(errClosing error) error {
	if s.connClosing {
		return errClosing
	}
	if s.readErr != nil {  // ⚠️ Once set, NEVER cleared
		return fmt.Errorf("%w: %v", errClosing, s.readErr)
	}
	if s.writeErr != nil {
		return fmt.Errorf("%w: %v", errClosing, s.writeErr)
	}
	return nil
}
```

#### 3. All Future Calls Rejected (Lines 328-375)

```go
func (c *Connection) Call(ctx context.Context, method string, params any) *AsyncCall {
	// ...
	c.updateInFlight(func(s *inFlightState) {
		err = s.shuttingDown(ErrClientClosing)  // 🚫 Returns ErrClientClosing
		if err != nil {
			return  // ❌ BLOCKS the call
		}
		// ... code never reached
	})

	if err != nil {
		ac.retire(&Response{ID: id, Error: err})
		return ac
	}
	// ...
}
```

### Design Flaw

**The SDK treats any read error as fatal and permanent**, with no recovery mechanism. There is no way to:
- Clear the `readErr` flag
- Reset the connection state
- Recover from transient network issues

### Error Classification: What Should Be Recoverable

To fix this bug properly, errors must be classified into **transient/recoverable** vs **permanent/fatal**:

#### ✅ Recoverable Errors (Should NOT Poison Connection)

These errors are temporary and the connection can continue after retries:

1. **Network Timeouts:**
   - `http.Client.Timeout` exceeded
   - `context.DeadlineExceeded` on per-call context
   - Load balancer timeout
   - **Reason**: Server may be temporarily slow, network congested

2. **Server Overload/Restart:**
   - 503 Service Unavailable
   - 502 Bad Gateway
   - Connection refused (server restarting)
   - Connection reset by peer (server deployment)
   - **Reason**: Server is temporarily unavailable but will recover

3. **Temporary Network Issues:**
   - DNS temporary failure (SERVFAIL, timeout)
   - Network unreachable (transient routing issue)
   - TLS handshake timeout (not certificate errors)
   - **Reason**: Network conditions may improve

4. **Rate Limiting:**
   - 429 Too Many Requests
   - **Reason**: Client should back off and retry

#### ❌ Fatal Errors (Should Poison Connection)

These errors indicate the session is permanently broken:

1. **Authentication/Authorization:**
   - 401 Unauthorized
   - 403 Forbidden
   - TLS certificate validation failure
   - **Reason**: Credentials are invalid, reconnection won't help

2. **Session Terminated:**
   - 404 Not Found (when session ID provided)
   - **Reason**: Server explicitly ended the session

3. **Protocol Errors:**
   - Invalid JSON in messages
   - Unsupported protocol version
   - Schema validation failures
   - **Reason**: Code bug or incompatibility, won't resolve without changes

4. **Client Errors:**
   - 400 Bad Request
   - 405 Method Not Allowed
   - Malformed requests
   - **Reason**: Client code bug, won't resolve by retrying

#### ⚠️ Ambiguous Cases

Some errors could be either transient or permanent depending on context:

1. **500 Internal Server Error:**
   - Could be: Transient bug triggered by specific input (recoverable)
   - Could be: Persistent server bug (fatal until server is fixed)
   - **Recommendation**: Treat as recoverable with limited retries

2. **Connection refused:**
   - Could be: Server restarting (recoverable)
   - Could be: Server permanently down or wrong endpoint (fatal)
   - **Recommendation**: Treat as recoverable with backoff

3. **DNS failures:**
   - Could be: Temporary DNS server issue (recoverable)
   - Could be: Wrong hostname configured (fatal)
   - **Recommendation**: Treat as recoverable initially

**Key Insight:** When in doubt, treat as recoverable with **exponential backoff and maximum retry limits**. This provides resilience without infinite loops.

---

## Stateless Mode Is Also Affected

**Critical Finding:** Stateless mode does **NOT** avoid this bug.

**Test Results:** `connection_closed_test.go`

Both tests demonstrate identical failures:

**Stateful Server Mode:**
```
Call #1: SUCCESS
Call #2: FAILED (timeout)
Call #3: FAILED - "client is closing: EOF"
```

**Stateless Server Mode:**
```
Call #1: SUCCESS
Call #2: FAILED (timeout)
Call #3: FAILED - "client is closing: EOF"
```

**Why Stateless Doesn't Help:**

1. **Server stateless mode** means:
   - Server doesn't validate Session-ID
   - Server creates temporary session per request
   - Server doesn't maintain connection state

2. **Client STILL maintains connection:**
   - Client reuses `streamableClientConn` object
   - Client reuses `jsonrpc2.Connection`
   - Client maintains Session-ID (even if server ignores it)
   - **Client connection persists across requests**

3. **Bug is in client connection layer:**
   - `readErr` is set in `jsonrpc2.Connection` (client-side)
   - Server mode doesn't affect client connection reuse
   - Error breaks client connection regardless of server mode

**Conclusion:** Both modes fail identically because the bug is in the **client's connection reuse**, not server behavior. The client SDK always maintains a persistent connection that gets poisoned by transient errors.

---

## Affected Components

### Maxwell Internal Components

| Component | File | Impact |
|-----------|------|--------|
| MCP Client | `internal/mcp/meli_mcp_client.go` | Direct impact - manages SDK sessions |
| Client Pool | `internal/mcp/pool.go` | Indirect - caches broken clients |
| Tool Handler | `internal/agent/tools/handler.go` | Indirect - tool calls fail |

### SDK Components

| Component | File | Issue |
|-----------|------|-------|
| Connection | `internal/jsonrpc2/conn.go` | **Root cause** - permanent `readErr` |
| Streamable Client | `mcp/streamable.go` | Reuses broken Connection |
| HTTP Client | `mcp/client.go` | Wraps broken Connection |

---

## Reproduction

### Minimal Example

```go
// 1. Create client and connect
client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
transport := &mcp.StreamableClientTransport{
	Endpoint:   serverURL,
	HTTPClient: &http.Client{Timeout: 2 * time.Second},
}
session, _ := client.Connect(ctx, transport, nil)

// 2. First call succeeds
result1, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "tool1"})
// ✅ SUCCESS

// 3. Second call times out (server slow, network issue, etc.)
result2, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "slow_tool"})
// ❌ TIMEOUT: "Client.Timeout exceeded while awaiting headers"

// 4. Third call fails even with valid context
result3, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "tool3"})
// ❌ ERROR: "client is closing"
// Session is now permanently broken
```

### Full Test Suite

Run the reproduction tests:

```bash
go test -v ./internal/mcp -run TestTimeout
```

**Expected Results:**
- `TestTimeoutBugReproduction` (stateful): FAIL - confirms bug
- `TestTimeoutStateless` (stateless): FAIL - confirms bug affects both modes

### Error Messages

When connection becomes unusable, you'll see:
- `"client is closing"`
- `"connection closed"`
- `"client is closing: EOF"`
- `"client is closing: session not found"`

---

## Environment Details

### SDK Version
```
github.com/modelcontextprotocol/go-sdk v1.0.0
```

### Toolkit Version
```
github.com/melisource/fury_go-toolkit-mcp (wraps official SDK)
```

### Go Version
```
go 1.23+
```

### MCP Server Implementations

Maxwell connects to MCP servers using **two different implementations**:

#### 1. Official SDK Servers
```
Built with: github.com/modelcontextprotocol/go-sdk
- Supports: Both stateful and stateless modes
- Status: ❌ Affected by client-side bug
```

#### 2. mark3labs Servers
```
Built with: github.com/mark3labs/mcp-go
- Repository: https://github.com/mark3labs/mcp-go
- Supports: Stateful mode only
- Status: ❌ Affected by client-side bug
```

**Both implementations are affected** because the bug is in the client SDK, not the server.

---

## Known Issues in Upstream

### Related GitHub Issues

Searched extensively for similar reports. **This specific bug has NOT been reported.**

#### Related (But Different) Issues:

1. **[Issue #148: Session recovery / shared session storage](https://github.com/modelcontextprotocol/go-sdk/issues/148)**
   - Status: Open (Feature Request)
   - Description: Proposes session recovery for distributed environments
   - **Different:** About distributed session storage, not timeout recovery
   - Relevance: Mentions lack of session recovery mechanisms

2. **[Issue #129: StreamableHttpClient Transport hangs on connection](https://github.com/modelcontextprotocol/go-sdk/issues/129)**
   - Status: Closed in v0.3.0 (PR #181)
   - Description: Client didn't support JSON responses
   - **Different:** Missing feature, not a timeout bug

3. **[Issue #224: Server.Run does not stop when context is cancelled](https://github.com/modelcontextprotocol/go-sdk/issues/224)**
   - Status: Open
   - Description: Context cancellation doesn't close connection
   - **Different:** Server-side issue, not client timeout recovery

4. **[Issue #285: sampling/createMessage hangs](https://github.com/modelcontextprotocol/go-sdk/issues/285)**
   - Status: Closed (PR #293)
   - Description: Cancellation handling bug causing hangs
   - **Different:** Cancellation stubs, not readErr persistence

### Upstream Status

**As of 2025-12-03:**
- ❌ **NOT reported** - The specific `readErr` permanent state bug has not been filed
- ❌ **NOT fixed** - No fix exists in v1.0.0 or later releases
- ⚠️ **Known limitation** - Session recovery is acknowledged as missing (#148)

**Recommendation:** File a new GitHub issue documenting this bug with:
- Clear reproduction case
- Root cause analysis (readErr never cleared)
- Suggested fix (clear readErr on retry or connection reset)
- Reference to our test cases

**Issue Created:** [#1246 - Analizar SDK oficial de MCP y evaluar reportar bug de reconexión upstream](https://github.com/melisource/fury_maxwell/issues/1246)

---

## Why This Bug Exists

### Architecture Decision

The SDK makes a conscious choice to:
1. **Reuse connections for efficiency** (avoid overhead of establishing new connections)
2. **Treat errors as fatal** (fail-fast philosophy)
3. **Delegate recovery to application layer** (SDK doesn't auto-reconnect)

### The Problem

This works for:
- ✅ Permanent failures (server down, invalid credentials)
- ✅ Explicit connection close (user-initiated)

But fails for:
- ❌ **Transient timeouts** (temporary network issues)
- ❌ **Slow server responses** (exceeds timeout but server is healthy)
- ❌ **Brief network interruptions** (recoverable errors)

### Why `readErr` Is Never Cleared

Looking at the SDK code, there's no mechanism to:
- Reset `readErr` after it's set
- Distinguish transient vs permanent errors
- Attempt connection recovery
- Reinitialize the read loop

The `inFlightState` structure is designed for one-way state transitions:
```
healthy → error → closed
```

There's no path back to healthy.

---

## Error Detection

### How to Identify the Issue

When this bug occurs, you'll see:

**In Logs:**
```
ERROR: failed to call tool 'mcp_server_tool': client is closing
ERROR: failed to list tools: connection closed
ERROR: connection closed: calling "tools/call": client is closing: EOF
```

**In Metrics:**
- `mcp.tool.call.error` spike after timeout events
- `mcp.tool.retry` attempts fail
- `mcp.pool.refresh.error` if caching broken clients

**In User Experience:**
- MCP tools stop working mid-conversation
- "Tool unavailable" errors
- User must start new conversation

### Debug Commands

```bash
# Check for timeout errors in logs
fury logs test | grep -i "timeout\|client is closing\|connection closed"

# Check MCP server status
curl -H "Accept: application/json" \
     -H "X-Caller-Id: 12345" \
     https://maxwell-test.furycloud.io/api/v1/mcp
```

---

## Solution Approach

**See:** Implementation in `internal/mcp/meli_mcp_client.go:252-276`

The workaround involves:

### 1. Error Detection

```go
func (c *meliMCPClient) isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	// The SDK's internal ErrClientClosing is not exported, so we check the message
	errMsg := err.Error()
	return strings.Contains(errMsg, "client is closing") ||
		strings.Contains(errMsg, "connection closed")
}
```

### 2. Thread-Safe Reconnection

```go
// reconnectSession reconnects to the MCP server.
// Old session is passed in to avoid race condition.
func (c *meliMCPClient) reconnectSession(ctx context.Context, brokenSession client.MeliMCPClientSession) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.session != brokenSession {
		return nil  // Another goroutine already reconnected
	}

	return c.Connect(ctx)
}
```

### 3. Automatic Retry

```go
func (c *meliMCPClient) CallTool(ctx context.Context, toolName string, arguments map[string]any, idempotencyKey string, meta map[string]any) (*mcp.CallToolResult, error) {
	// ... setup ...

	session := c.session  // Capture current session
	result, err := session.CallTool(ctx, params)

	// Detect and recover from connection closed error
	if c.isConnectionClosedError(err) {
		if reconnectErr := c.reconnectSession(ctx, session); reconnectErr == nil {
			// Retry with new session
			result, err = c.session.CallTool(ctx, params)
			if err == nil {
				return result, nil
			}
		}
	}

	return result, err
}
```

### Why This Works

1. **Detects broken connections** via error message matching
2. **Thread-safe reconnection** using mutex and pointer comparison
3. **Automatic retry** transparent to caller
4. **Idempotent** - safe to call multiple times
5. **Works for all operations** - ListTools and CallTool

**Note:** This is an application-layer workaround. The proper fix should be in the SDK itself.

---

## Recommendations

### For Upstream SDK (modelcontextprotocol/go-sdk)

1. **File GitHub Issue** - Document this bug in the official repository
   - Title: "StreamableClientTransport: Connection becomes permanently unusable after timeout"
   - Include: Reproduction case, root cause, suggested fix
   - Reference: This document and test cases

2. **Propose Fix Options:**

   **Option A: Clear readErr on new request**
   ```go
   func (c *Connection) Call(...) {
       // Reset readErr if connection appears healthy
       c.updateInFlight(func(s *inFlightState) {
           if s.readErr != nil && !s.connClosing {
               s.readErr = nil  // Clear transient errors
           }
       })
   }
   ```

   **Option B: Implement connection reset**
   ```go
   func (c *Connection) Reset() error {
       // Restart read loop, clear errors
   }
   ```

   **Option C: Auto-reconnect on transient errors**
   ```go
   func (c *Connection) shouldRetry(err error) bool {
       // Distinguish transient vs permanent errors
   }
   ```

3. **Add Tests** - Timeout recovery should be tested
4. **Documentation** - Warn users about session recovery limitations

### For Maxwell

1. ✅ **Workaround Implemented** - Automatic reconnection for all MCP clients
2. ✅ **Test Coverage Added** - Tests confirm bug and verify workaround
3. ✅ **Spec Documented** - Complete analysis in `connection_closed.md`
4. ✅ **Issue Created** - #1246 to track upstream reporting
5. 🔄 **Monitor Upstream** - Watch for official SDK fix in future releases
6. 📊 **Track Metrics** - Monitor reconnection frequency
7. 🧪 **Expand Testing:**
   - Add concurrent timeout scenarios
   - Test multiple consecutive timeouts
   - Test timeout during different operations (ListTools, CallTool, etc.)

### For mark3labs MCP Servers

**Note**: Stateless mode does **NOT** fix this bug (it's a client-side issue).

Both mark3labs servers (stateful) and official SDK servers (stateless) are equally affected because:
- The bug is in the client SDK's connection reuse
- Server mode doesn't affect client connection management
- Client reconnection logic is required regardless of server implementation

**Reference:** https://github.com/mark3labs/mcp-go

---

## References

### Official Documentation

- [MCP Go SDK Repository](https://github.com/modelcontextprotocol/go-sdk)
- [Release v1.0.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.0.0)
- [MCP Specification - Streamable HTTP Transport](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)

### Related Issues (Different Bugs)

- [Issue #148: Session recovery proposal](https://github.com/modelcontextprotocol/go-sdk/issues/148) - Distributed session storage (not timeout recovery)
- [Issue #129: StreamableHttpClient Transport hangs](https://github.com/modelcontextprotocol/go-sdk/issues/129) - Missing JSON support (fixed)
- [Issue #224: Server.Run context cancellation](https://github.com/modelcontextprotocol/go-sdk/issues/224) - Server-side issue
- [Issue #285: sampling/createMessage hangs](https://github.com/modelcontextprotocol/go-sdk/issues/285) - Cancellation bug (fixed)

### SDK Source Files

- [`internal/jsonrpc2/conn.go`](https://github.com/modelcontextprotocol/go-sdk/blob/main/internal/jsonrpc2/conn.go) - Connection management (root cause)
- [`mcp/streamable.go`](https://github.com/modelcontextprotocol/go-sdk/blob/main/mcp/streamable.go) - HTTP transport (client connection reuse)
- [`mcp/client.go`](https://github.com/modelcontextprotocol/go-sdk/blob/main/mcp/client.go) - Client API

### Maxwell Files

- [`internal/mcp/meli_mcp_client.go`](internal/mcp/meli_mcp_client.go) - MCP client implementation (workaround)
- [`internal/mcp/timeout_bug_reproduction_test.go`](internal/mcp/timeout_bug_reproduction_test.go) - Bug reproduction tests
- [`internal/mcp/pool.go`](internal/mcp/pool.go) - Client pooling
- [`internal/agent/tools/handler.go`](internal/agent/tools/handler.go) - Tool execution

### Maxwell Issues

- [Issue #1246: Analizar SDK oficial de MCP y evaluar reportar bug upstream](https://github.com/melisource/fury_maxwell/issues/1246) - Task to report this bug upstream

---

## Last Updated

**Date:** 2025-12-03
**SDK Version:** v1.0.0
**Status:** Workaround active, upstream issue tracking created (#1246)

**Key Findings:**
- ✅ Bug confirmed in both stateful and stateless modes
- ✅ Root cause: Client-side connection reuse + permanent readErr
- ✅ Affects ALL servers (not just mark3labs)
- ❌ NOT reported upstream yet (tracking in #1246)
- ✅ Workaround implemented and tested
