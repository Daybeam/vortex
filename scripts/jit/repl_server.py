#!/usr/bin/env python3
"""JIT Session REPL server (POC, see playbook/jit-session-poc-design-2026.md).

Speaks the exact JSON-RPC-lite wire format core/client/client.go's
MCPClient.SendRequest/readLoop already implement (request:
{"jsonrpc":"2.0","method":...,"params":...,"id":...}, response:
{"jsonrpc":"2.0","id":...,"result":...} or {..., "error":{"code":...,"message":...}}),
so the Go side can reuse client.NewMCPClient + MCPClient.SendRequest directly
without inventing a new client type or dispatch protocol. Deliberately does
NOT implement the MCP "initialize" handshake -- this is not a real MCP tool
server, it's a private session channel, so the Go side must call
client.NewMCPClient directly (bypassing ensureMCPClient/Initialize) rather
than going through the normal MCP-tool spawn path.

Supported methods:
  "eval"  params={"code": "..."} -> result={"stdout":..., "stderr":..., "result":...}
           Executes code against a single, persistent globals() dict shared
           across every eval call for this process's lifetime. Code may
           optionally assign __result__ to return a JSON-serializable value.
  "ping"  params={} -> result={"ok": true}
           Cheap liveness check, no side effects on _globals.
"""
import sys
import json
import io
import contextlib

_globals = {}


def _handle(req):
    req_id = req.get("id")
    method = req.get("method")
    params = req.get("params") or {}

    if method == "ping":
        return {"jsonrpc": "2.0", "id": req_id, "result": {"ok": True}}

    if method != "eval":
        return {"jsonrpc": "2.0", "id": req_id,
                "error": {"code": -32601, "message": "unknown method: %r" % (method,)}}

    code = params.get("code", "")
    out, err = io.StringIO(), io.StringIO()
    result = None
    try:
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            exec(compile(code, "<jit_eval>", "exec"), _globals)
            result = _globals.get("__result__")
    except Exception as e:
        err.write(str(e))

    try:
        json.dumps(result)
    except TypeError:
        # __result__ wasn't JSON-serializable; report as a string instead of
        # failing the whole eval (stdout/stderr are still returned intact).
        result = repr(result)

    return {
        "jsonrpc": "2.0",
        "id": req_id,
        "result": {"stdout": out.getvalue(), "stderr": err.getvalue(), "result": result},
    }


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            resp = {"jsonrpc": "2.0", "id": None,
                    "error": {"code": -32700, "message": "parse error: %s" % e}}
        else:
            try:
                resp = _handle(req)
            except Exception as e:  # never let a bug here kill the session silently
                resp = {"jsonrpc": "2.0", "id": req.get("id"),
                        "error": {"code": -32603, "message": "internal error: %s" % e}}
        sys.stdout.write(json.dumps(resp) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
