## PR title

Streamable client: prevent global HTTP timeouts from poisoning connections; add guardrails, tests, and docs


## Summary

- Fix class of “client is closing” errors caused by `http.Client.Timeout` aborting the long‑lived SSE GET and/or POST, which permanently sets `readErr`/`writeErr` in jsonrpc2.
- Add guardrail: reject non‑zero `HTTPClient.Timeout` in `StreamableClientTransport`.
- Make call‑level timeouts non‑fatal: treat POST timeouts as “rejected” writes so the connection remains healthy.
- Add recovery tests and usage docs recommending per‑call context deadlines instead of global client timeouts.


## Background and spec context

- Streamable HTTP relies on a hanging GET (SSE) that must not be bounded by a short global client timeout. See “Listening for messages from the server” in the spec: [Listening for messages from the server](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#listening-for-messages-from-the-server).
- Related server cleanup discussion: “Export callback to StreamableHTTPHandler for closed transports” [#479](https://github.com/modelcontextprotocol/go-sdk/issues/479).


## Reproduction

Minimal repro (stateful or stateless):
1) Call #1 succeeds.
2) Call #2 sleeps longer than the client timeout; it times out.
3) Call #3 should succeed but fails with “client is closing”, and all subsequent calls fail until reconnect.

Root observation: setting `HTTPClient.Timeout > 0` applies to both POST and the hanging SSE GET. When it fires, jsonrpc2 records a terminal `readErr`/`writeErr` and refuses new work.


## Root cause in code

- Reader failure poisons the connection (SSE GET timeout → `readErr` set):

```560:570:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/internal/jsonrpc2/conn.go
	c.updateInFlight(func(s *inFlightState) {
		s.reading = false
		s.readErr = err

		// Retire any outgoing requests that were still in flight: with the Reader no
		// longer being processed, they necessarily cannot receive a response.
		for id, ac := range s.outgoingCalls {
			ac.retire(&Response{ID: id, Error: err})
		}
		s.outgoingCalls = nil
	})
```

- Writer failure poisons the connection (POST under global Timeout → `writeErr` set):

```781:799:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/internal/jsonrpc2/conn.go
	if err != nil && ctx.Err() == nil {
		// The writer appears to be broken, and future writes are likely to also fail.
		c.updateInFlight(func(s *inFlightState) {
			if s.writeErr == nil {
				s.writeErr = err
				for _, r := range s.incomingByID {
					r.cancel()
				}
			}
		})
	}
```

- New work is rejected after either error:

```166:176:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/internal/jsonrpc2/conn.go
func (s *inFlightState) shuttingDown(errClosing error) error {
	if s.connClosing {
		return errClosing
	}
	if s.readErr != nil {
		return fmt.Errorf("%w: %v", errClosing, s.readErr)
	}
	if s.writeErr != nil {
		return fmt.Errorf("%w: %v", errClosing, s.writeErr)
	}
	return nil
}
```

- POST and SSE GET both use the same `HTTPClient` (global Timeout applies to both):

```1559:1570:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/mcp/streamable.go
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %v", requestSummary, err)
	}
```

```1828:1843:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/mcp/streamable.go
			req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url, nil)
			// ...
			resp, err := c.client.Do(req)
			if err != nil {
				finalErr = err // Store the error and try again.
				delay = calculateReconnectDelay(attempt + 1)
				continue
			}
```

- Client attempts DELETE on Close (explains seeing DELETE even in some stateless flows):

```1853:1875:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/mcp/streamable.go
func (c *streamableClientConn) Close() error {
	c.closeOnce.Do(func() {
		if errors.Is(c.failure(), errSessionMissing) {
			// If the session is missing, no need to delete it.
		} else {
			req, err := http.NewRequestWithContext(c.ctx, http.MethodDelete, c.url, nil)
			// ...
			if _, err := c.client.Do(req); err != nil {
				c.closeErr = err
			}
		}
		// Cancel any hanging network requests after cleanup.
		c.cancel()
		close(c.done)
	})
	return c.closeErr
}
```

- Server requires `Mcp-Session-Id` for DELETE (returns 400 if missing):

```242:253:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/mcp/streamable.go
	if req.Method == http.MethodDelete {
		if sessionID == "" {
			http.Error(w, "Bad Request: DELETE requires an Mcp-Session-Id header", http.StatusBadRequest)
			return
		}
		if sessInfo != nil {
			sessInfo.session.Close()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
```

- Handler may still assign a Session-ID on initialize (even with stateless handler option):

```314:322:/Users/mibar/.cursor/worktrees/mcp-go-sdk/gpf/mcp/streamable.go
		if sessionID == "" {
			// In stateless mode, sessionID may be nonempty even if there's no
			// existing transport.
			sessionID = server.opts.GetSessionID()
		}
```


## Proposed changes

1) Guardrail: reject global client timeouts for streamable transport

- In `StreamableClientTransport.Connect`, return an error if `HTTPClient != nil` and `HTTPClient.Timeout > 0` with guidance to use per‑call `context.WithTimeout`.

Example:

```go
// mcp/streamable.go (in Connect)
if client.Timeout > 0 {
	return nil, fmt.Errorf("StreamableClientTransport: HTTPClient.Timeout must be 0 for streamable transport; use per-call context deadlines instead")
}
```

2) Make call-level timeouts non‑fatal to the connection

- In `streamableClientConn.Write`, if `c.client.Do(req)` returns a timeout (`net.Error.Timeout()` or `context.DeadlineExceeded`), wrap as `jsonrpc2.ErrRejected`. jsonrpc2 treats “rejected” writes as non‑fatal (does not set `writeErr`).

Example:

```go
// mcp/streamable.go (in Write)
resp, err := c.client.Do(req)
if err != nil {
	if nerr, ok := err.(net.Error); ok && nerr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", jsonrpc2.ErrRejected, err)
	}
	return fmt.Errorf("%s: %v", requestSummary, err)
}
```

3) Docs and example updates

- In `docs/client.md` and `examples/client/*`, demonstrate:
  - `HTTPClient.Timeout = 0`
  - Per‑call timeouts via `context.WithTimeout` on `CallTool`.

4) Tests

- Add `mcp/timeout_recovery_test.go` verifying:
  - With `HTTPClient.Timeout=0` and per-call deadlines, a timed‑out call does not break the session; the next call succeeds (both stateful and stateless handlers).
  - With `HTTPClient.Timeout>0`, `Connect` fails fast (guardrail).

5) Follow‑ups (separate PRs)

- Server lifecycle polish aligned with [#479](https://github.com/modelcontextprotocol/go-sdk/issues/479): add `StreamableServerTransport.Close()` and invoke it on DELETE to ensure full cleanup, and optionally expose `OnConnectionClose` in handler options.


## Backward compatibility

- Guardrail is a behavior change: clients relying on `HTTPClient.Timeout>0` will now get a clear error instead of latent connection poisoning.
- No protocol changes. Per‑call deadlines remain fully supported and recommended.


## Risks and mitigations

- Misclassifying write errors as timeouts: only treat `net.Error.Timeout()` or `context.DeadlineExceeded` as non‑fatal; other errors continue to fail the connection.
- Some users expect global timeouts; error messaging directs them to per‑call deadlines or transport-level knobs like `ResponseHeaderTimeout`.


## Acceptance criteria

- Repro test passes with `HTTPClient.Timeout=0` and per‑call deadline: Call #3 succeeds after a timed‑out Call #2 (stateful and stateless handlers).
- Guardrail test: `Connect` fails fast if `Timeout>0`.
- No regressions in existing conformance/e2e tests.


## Checklist

- [ ] Guardrail in `StreamableClientTransport.Connect`
- [ ] Timeout-as-rejected handling in `streamableClientConn.Write`
- [ ] Tests: recovery + guardrail
- [ ] Docs: guidance and examples updated
- [ ] Consider follow‑up for server-side close per [#479](https://github.com/modelcontextprotocol/go-sdk/issues/479)


## Usage snippet (docs)

```go
// Correct usage with streamable transport:
httpClient := &http.Client{ Timeout: 0 } // do not set a global timeout
transport := &mcp.StreamableClientTransport{
    Endpoint:   serverURL,
    HTTPClient: httpClient,
}

ctx := context.Background()
session, _ := client.Connect(ctx, transport, nil)

// Bound each call individually
callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
_, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: "delay_tool"})
```


## References

- Spec: [Listening for messages from the server](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#listening-for-messages-from-the-server)
- Issue: [Proposal: Export callback to StreamableHTTPHandler for closed transports #479](https://github.com/modelcontextprotocol/go-sdk/issues/479)