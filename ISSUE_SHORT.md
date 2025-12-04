# StreamableClientTransport: Connection poisoned by transient errors

## Summary

The `StreamableClientTransport` permanently breaks after **any transient error** (timeout, 503, network interruption), making the connection unusable for all subsequent requests.

## Reproduction

```go
httpClient := &http.Client{Timeout: 2 * time.Second}
session, _ := client.Connect(ctx, transport, nil)

result1, _ := session.CallTool(ctx, ...)  // ✅ SUCCEEDS
result2, _ := session.CallTool(ctx, ...)  // ⏱️ TIMES OUT (slow server)
result3, _ := session.CallTool(ctx, ...)  // ❌ FAILS: "client is closing"
```

## Root Cause

`streamableClientConn.Write()` doesn't use `jsonrpc2.ErrRejected`, causing `writeErr` to be set permanently in `jsonrpc2.Connection`.

## Proposed Fix (6 lines)

```go
resp, err := c.client.Do(req)
if err != nil {
    return fmt.Errorf("%s: %w: %v", requestSummary, jsonrpc2.ErrRejected, err)
}

if err := c.checkResponse(requestSummary, resp); err != nil {
    return fmt.Errorf("%w: %v", jsonrpc2.ErrRejected, err)
}
```

## Test Results

**Before fix**: Call #3 fails with "client is closing"
**After fix**: Call #3 succeeds ✅

See full analysis in: `ISSUE_TEMPLATE.md`, `connection_cosed.md`, `connection_timeout_issue.md`

Branch: `fix/connection-poisoning-low-level`
