#!/usr/bin/env node
/**
 * relay adapter (docs 6.2.1 / 6.7) — dev data plane for ONE account.
 *
 * Drives a real Claude Code binary via @anthropic-ai/claude-agent-sdk streaming-input
 * (one long-lived CC child process per conversation session, stdin held open), and
 * exposes the gateway<->instance contract:
 *
 *   POST /v1/invoke        { session_key, model?, betas?, message } ->
 *                          SSE: relay_start -> raw Anthropic events -> relay_control
 *   POST /cancel/{id}      aborts the in-flight turn (interrupt) — F11 mandatory
 *   GET  /status           sessions + busy state
 *   GET  /healthz          liveness
 *
 * What this adapter guarantees on the wire (fleet rules that still apply in dev):
 *   - the REAL claude executable mints every request byte: claude_code system preset,
 *     tool schemas, metadata.user_id, cch, x-stainless-*, telemetry — nothing is
 *     synthesized here, and bodies are never rewritten (6.1.2: any body rewrite
 *     invalidates cch).
 *   - one CC child process = one session_id (6.3); turns within a conversation share
 *     it; adapter restarts resume the same session_id from sessions.json.
 *   - the child env is replaced with an explicit allowlist (6.4), never inherited
 *     wholesale; dangerous vars (ANTHROPIC_BASE_URL / API_KEY / AUTH_TOKEN /
 *     CUSTOM_HEADERS / NO_PROXY …) are asserted absent.
 *
 * Usage:
 *   npm install
 *   node adapter.mjs --listen tcp://127.0.0.1:7301 \
 *       --claude "C:\\Users\\<you>\\.local\\bin\\claude.exe" [--cwd <workspace>]
 *
 * Point the gateway at it via config.yaml:
 *   relay: { enabled: true, agents: { "<auth-id>": "tcp://127.0.0.1:7301" } }
 */
import http from "node:http";
import net from "node:net";
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { fileURLToPath } from "node:url";
import { query } from "@anthropic-ai/claude-agent-sdk";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------- CLI parsing
function parseArgs(argv) {
  const out = { listen: "tcp://127.0.0.1:7301", claude: "", cwd: process.cwd(), store: path.join(__dirname, "sessions.json") };
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i];
    const next = () => argv[++i];
    if (a === "--listen") out.listen = next();
    else if (a === "--claude") out.claude = next();
    else if (a === "--cwd") out.cwd = next();
    else if (a === "--store") out.store = next();
    else if (a === "--help" || a === "-h") {
      console.log("node adapter.mjs [--listen tcp://127.0.0.1:7301|unix:///path.sock] [--claude /abs/path/claude] [--cwd dir] [--store sessions.json]");
      process.exit(0);
    }
  }
  return out;
}
const args = parseArgs(process.argv);

// ------------------------------------------------- env allowlist (docs 6.4)
// The child env is REPLACED, never inherited. Fleet preflight asserts the dangerous
// set is absent — same assertion here, at adapter boot.
const FORBIDDEN_ENV = [
  "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY",
  "ANTHROPIC_CUSTOM_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS",
  "CLAUDE_CODE_EXTRA_METADATA", "USE_STAGING_OAUTH", "CLAUDE_CODE_CUSTOM_OAUTH_URL",
  "CLAUDE_CODE_HOST_PLATFORM", "NO_PROXY", "no_proxy", "CLAUDE_CODE_PROXY_RESOLVES_HOSTS",
];
for (const name of FORBIDDEN_ENV) {
  if (process.env[name] !== undefined) {
    console.error(`[adapter] FATAL: ${name} is set in the adapter environment (docs 6.4 preflight: fatal correlator). Unset it and restart.`);
    process.exit(1);
  }
}
const ENV_ALLOWLIST = [
  "PATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "TEMP", "TMP", "COMSPEC",
  "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
  "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM", "LANG", "TZ",
  "CLAUDE_CONFIG_DIR", "HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy",
];
function childEnv() {
  const env = {};
  for (const name of ENV_ALLOWLIST) {
    if (process.env[name] !== undefined) env[name] = process.env[name];
  }
  return env;
}

// ------------------------------------------------- session store (key -> session_id)
// Restart recovery keeps the SAME session_id per conversation (6.3: resume semantics,
// never a fresh device/session churn).
function loadStore(file) {
  try { return JSON.parse(fs.readFileSync(file, "utf8")); } catch { return {}; }
}
function saveStore(file, data) {
  const tmp = file + ".tmp";
  fs.writeFileSync(tmp, JSON.stringify(data, null, 2));
  fs.renameSync(tmp, file);
}
const sessionIds = loadStore(args.store);

// ------------------------------------------------- streaming-input session driver
class CCSession {
  constructor(key) {
    this.key = key;
    this.sessionId = sessionIds[key] || crypto.randomUUID();
    sessionIds[key] = this.sessionId;
    saveStore(args.store, sessionIds);
    this.inbox = [];        // queued SDKUserMessage values
    this.inboxWaiter = null;
    this.q = null;
    this.iter = null;
    this.busy = false;
  }

  async *input() {
    // stdin stays open for the life of the process (streaming-input mode).
    for (;;) {
      const msg = await this._take();
      if (msg === null) return;
      yield msg;
    }
  }

  _take() {
    if (this.inbox.length) return Promise.resolve(this.inbox.shift());
    return new Promise((resolve) => { this.inboxWaiter = resolve; });
  }

  push(message) {
    const sdkMsg = {
      type: "user",
      message, // { role: "user", content: <string | blocks> } passed through untouched
      parent_tool_use_id: null,
      session_id: this.sessionId,
    };
    if (this.inboxWaiter) {
      const w = this.inboxWaiter;
      this.inboxWaiter = null;
      w(sdkMsg);
    } else {
      this.inbox.push(sdkMsg);
    }
  }

  ensure() {
    if (this.q) return this.q;
    const opts = {
      cwd: args.cwd,
      env: childEnv(),
      includePartialMessages: true, // surface the raw Anthropic SSE inside stream_event
      forkSession: false,           // continue the same session_id, never fork (6.3)
      permissionMode: "acceptEdits",
      resume: this.sessionId,       // stable session across adapter restarts
    };
    if (args.claude) opts.pathToClaudeCodeExecutable = args.claude; // never PATH-discover (6.2.2)
    this.q = query({ prompt: this.input(), options: opts });
    this.iter = this.q[Symbol.asyncIterator]();
    return this.q;
  }
}

const sessions = new Map();          // session_key -> CCSession
const streamOwners = new Map();      // stream_id -> CCSession
function sessionFor(key) {
  let s = sessions.get(key);
  if (!s) { s = new CCSession(key); sessions.set(key, s); }
  return s;
}

// ------------------------------------------------- /v1/invoke
async function handleInvoke(req, res) {
  const body = await readJson(req);
  const { session_key: key, model, message } = body || {};
  if (!key || typeof key !== "string") return sendJson(res, 400, { error: "session_key required" });
  if (!message || message.role !== "user") return sendJson(res, 400, { error: "message must be a user message" });

  const sess = sessionFor(key);
  if (sess.busy) {
    // The gateway serializes per account (concurrency=1); a busy session means a
    // contract violation — refuse rather than interleave turns into one CC session.
    return sendJson(res, 409, { error: "session busy: one turn in flight (strict serialization)" });
  }
  sess.busy = true;

  const streamId = crypto.randomUUID();
  streamOwners.set(streamId, sess);

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache",
    Connection: "close", // docs H5: no keep-alive pooling on the agent channel
  });
  const sse = (event, data) => {
    res.write(`event: ${event}\n`);
    for (const line of JSON.stringify(data).split("\n")) res.write(`data: ${line}\n`);
    res.write("\n");
  };
  sse("relay_start", { stream_id: streamId, agent_session_id: sess.sessionId });

  let finished = false;
  const finish = (control) => {
    if (finished) return;
    finished = true;
    sess.busy = false;
    streamOwners.delete(streamId);
    sse("relay_control", control);
    res.end();
  };

  try {
    const q = sess.ensure();
    if (model && typeof q.setModel === "function") {
      try { await q.setModel(model); } catch { /* model passthrough is best-effort */ }
    }
    sess.push(message);

    // Iterate until this turn's `result` envelope (turn end).
    for (;;) {
      const { value: msg, done } = await sess.iter.next();
      if (done) throw new Error("CC session exited unexpectedly");
      if (msg.type === "stream_event" && msg.event) {
        // Raw Anthropic SSE, forwarded byte-faithfully (gateway passes to tenant).
        sse(msg.event.type || "message", msg.event);
      } else if (msg.type === "result") {
        finish({
          agent_session_id: sess.sessionId,
          usage: normalizeUsage(msg.usage),
          ratelimit: msg.modelUsage ? { model_usage: msg.modelUsage } : undefined,
          aborted: msg.subtype === "error_during_execution" ? false : undefined,
        });
        return;
      }
      // system init / assistant / user envelopes are internal to the agent loop;
      // the wire bytes live inside stream_event, so nothing else is forwarded.
    }
  } catch (err) {
    sse("error", { type: "error", error: { type: "agent_error", message: String(err && err.message || err) } });
    finish({ agent_session_id: sess.sessionId, usage: null });
  }
}

function normalizeUsage(u) {
  if (!u) return null;
  return {
    input_tokens: u.input_tokens ?? u.inputTokens ?? 0,
    output_tokens: u.output_tokens ?? u.outputTokens ?? 0,
    cache_read_input_tokens: u.cache_read_input_tokens ?? u.cacheReadInputTokens ?? 0,
  };
}

// ------------------------------------------------- cancel / status / healthz
async function handleCancel(res, streamId) {
  const sess = streamOwners.get(streamId);
  if (!sess || !sess.q) return sendJson(res, 404, { error: "unknown stream_id" });
  try { if (typeof sess.q.interrupt === "function") await sess.q.interrupt(); } catch { /* best effort */ }
  sendJson(res, 200, { ok: true });
}

function handleStatus(res) {
  sendJson(res, 200, {
    sessions: [...sessions.values()].map((s) => ({ key: s.key, session_id: s.sessionId, busy: s.busy })),
    cwd: args.cwd,
  });
}

// ------------------------------------------------- HTTP plumbing
function readJson(req) {
  return new Promise((resolve, reject) => {
    let raw = "";
    req.on("data", (c) => { raw += c; if (raw.length > 8 << 20) req.destroy(); });
    req.on("end", () => { try { resolve(JSON.parse(raw || "{}")); } catch (e) { reject(e); } });
    req.on("error", reject);
  });
}
function sendJson(res, code, obj) {
  const raw = JSON.stringify(obj);
  res.writeHead(code, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(raw) });
  res.end(raw);
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, "http://relay.local");
    if (req.method === "POST" && url.pathname === "/v1/invoke") return await handleInvoke(req, res);
    if (req.method === "POST" && url.pathname.startsWith("/cancel/")) return await handleCancel(res, decodeURIComponent(url.pathname.slice("/cancel/".length)));
    if (req.method === "GET" && url.pathname === "/status") return handleStatus(res);
    if (req.method === "GET" && url.pathname === "/healthz") return sendJson(res, 200, { ok: true });
    sendJson(res, 404, { error: "unknown route" });
  } catch (err) {
    sendJson(res, 500, { error: String(err && err.message || err) });
  }
});

// ------------------------------------------------- listen on tcp:// or unix://
function listen() {
  const m = args.listen.match(/^(tcp|unix):\/\/(.+)$/);
  if (!m) { console.error(`[adapter] bad --listen ${args.listen}`); process.exit(1); }
  if (m[1] === "unix") {
    const sockPath = m[2];
    try { fs.unlinkSync(sockPath); } catch { /* absent */ }
    server.listen(sockPath, () => console.log(`[adapter] listening on unix://${sockPath} (account agent, CC sessions in ${args.cwd})`));
  } else {
    const u = new URL("http://" + m[2]);
    const host = u.hostname;
    if (!["127.0.0.1", "::1", "localhost"].includes(host)) {
      console.error("[adapter] FATAL: tcp listen must be loopback (data plane is a local channel, docs 6.7)");
      process.exit(1);
    }
    server.listen(Number(u.port), host, () => console.log(`[adapter] listening on tcp://${host}:${u.port} (account agent, CC sessions in ${args.cwd})`));
  }
}
listen();
