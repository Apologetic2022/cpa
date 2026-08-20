import { query } from "@anthropic-ai/claude-agent-sdk";
import crypto from "node:crypto";

const mode = process.argv[2] || "with-resume";
const sessionId = crypto.randomUUID();
const ALLOW = ["PATH","HOME","USER","LOGNAME","SHELL","TERM","COLORTERM","LANG","TZ","CLAUDE_CONFIG_DIR","CLAUDE_CODE_OAUTH_TOKEN","HTTPS_PROXY","HTTP_PROXY","https_proxy","http_proxy"];
const env = {};
for (const k of ALLOW) if (process.env[k] !== undefined) env[k] = process.env[k];

async function* input() {
  yield {
    type: "user",
    message: { role: "user", content: "Reply with exactly: pong" },
    parent_tool_use_id: null,
    session_id: sessionId,
  };
  await new Promise(() => {});
}

const opts = {
  cwd: "/opt/cli-proxy/agents/workspace",
  env,
  includePartialMessages: true,
  forkSession: false,
  permissionMode: "acceptEdits",
  pathToClaudeCodeExecutable: "/opt/cli-proxy/bin/claude",
};
if (mode === "with-resume") opts.resume = sessionId;

console.log("mode:", mode, "sessionId:", sessionId);
const q = query({ prompt: input(), options: opts });
let n = 0;
try {
  for await (const msg of q) {
    n++;
    if (msg.type === "result") {
      console.log("RESULT:", JSON.stringify(msg).slice(0, 1500));
      break;
    }
    if (n <= 3 || msg.type === "system") console.log(msg.type + ":", JSON.stringify(msg).slice(0, 400));
  }
} catch (e) {
  console.log("ITER ERROR:", String(e && e.message || e).slice(0, 800));
}
console.log("messages seen:", n);
process.exit(0);
