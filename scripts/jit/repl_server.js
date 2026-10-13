#!/usr/bin/env node
/**
 * JIT Session REPL server — Node.js edition.
 *
 * Speaks the same JSON-RPC-lite wire format as repl_server.py:
 *   request:  {"jsonrpc":"2.0","method":...,"params":...,"id":...}
 *   response: {"jsonrpc":"2.0","id":...,"result":...} or
 *             {"jsonrpc":"2.0","id":...,"error":{"code":...,"message":...}}
 *
 * Supported methods:
 *   "eval"  params={"code":"..."} -> result={"stdout":...,"stderr":...,"result":...}
 *   "ping"  params={} -> result={"ok":true}
 *
 * State persists in a single global VM context across eval calls.
 * Uses Node's `vm` module for isolation (not a full sandbox, but keeps
 * variables alive between calls).
 */

const vm = require('vm');
const readline = require('readline');

// Persistent context — variables and imports survive across eval calls.
const ctx = { console, require, process, module, exports, __result__: undefined };
vm.createContext(ctx);

function handle(req) {
    const id = req.id;
    const method = req.method;
    const params = req.params || {};

    if (method === 'ping') {
        return { jsonrpc: '2.0', id, result: { ok: true } };
    }

    if (method !== 'eval') {
        return { jsonrpc: '2.0', id, error: { code: -32601, message: 'unknown method: ' + method } };
    }

    const code = params.code || '';
    let stdout = '';
    let stderr = '';
    let result = undefined;

    // Capture console.log/error output.
    const origLog = console.log;
    const origErr = console.error;
    console.log = (...args) => { stdout += args.join(' ') + '\n'; };
    console.error = (...args) => { stderr += args.join(' ') + '\n'; };

    try {
        // Run in the persistent context — variables persist across calls.
        vm.runInContext(code, ctx);
        result = ctx.__result__;
    } catch (e) {
        stderr += String(e);
    } finally {
        console.log = origLog;
        console.error = origErr;
    }

    // Reset __result__ for the next call.
    ctx.__result__ = undefined;

    return { jsonrpc: '2.0', id, result: { stdout, stderr, result } };
}

const rl = readline.createInterface({ input: process.stdin, terminal: false });

rl.on('line', (line) => {
    line = line.trim();
    if (!line) return;
    let resp;
    try {
        const req = JSON.parse(line);
        try {
            resp = handle(req);
        } catch (e) {
            resp = { jsonrpc: '2.0', id: req.id, error: { code: -32603, message: 'internal error: ' + e } };
        }
    } catch (e) {
        resp = { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error: ' + e } };
    }
    process.stdout.write(JSON.stringify(resp) + '\n');
});
