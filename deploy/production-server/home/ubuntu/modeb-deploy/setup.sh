#!/usr/bin/env bash
# CLIProxyAPI Mode-B relay — VPS setup (docs ch.8, dev single-account form).
# Installs: /opt/cli-proxy/{bin,adapter,agents,auths,logs,run}, Node.js 22 (official
# tarball, no curl|bash), systemd units for gateway + adapter template.
# Does NOT seed Claude credentials — that is an explicit operator step (see notes at end).
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "run as root: sudo bash setup.sh" >&2
  exit 1
fi

BASE=/opt/cli-proxy
NODE_VER=v22.17.0
NODE_DIST="node-${NODE_VER}-linux-x64"

echo "==> layout"
mkdir -p "$BASE"/{bin,adapter,agents,auths,logs,run}
cp -f "$(dirname "$0")/cli-proxy-api.linux-amd64" "$BASE/bin/cli-proxy-api"
chmod 0755 "$BASE/bin/cli-proxy-api"
cp -f "$(dirname "$0")/adapter/adapter.mjs" "$BASE/adapter/adapter.mjs"
cp -f "$(dirname "$0")/adapter/package.json" "$BASE/adapter/package.json"
[[ -f "$BASE/config.yaml" ]] || cp -f "$(dirname "$0")/config.yaml" "$BASE/config.yaml"

echo "==> node ${NODE_VER} (official tarball)"
if ! /usr/bin/node --version 2>/dev/null | grep -q "v22"; then
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/node.tar.xz" "https://nodejs.org/dist/${NODE_VER}/${NODE_DIST}.tar.xz"
  tar -xJf "$tmp/node.tar.xz" -C "$tmp"
  cp -f "$tmp/${NODE_DIST}/bin/node" /usr/bin/node
  chmod 0755 /usr/bin/node
  rm -rf "$tmp"
fi
node --version
ln -sf /usr/bin/node /usr/local/bin/node 2>/dev/null || true

echo "==> adapter deps"
cd "$BASE/adapter"
/usr/bin/node /usr/bin/npm --version >/dev/null 2>&1 || {
  # npm ships with the node tarball under lib/node_modules; wire it up
  if [[ -f /usr/lib/node_modules/npm/bin/npm-cli.js ]]; then
    printf '#!/usr/bin/env bash\nexec /usr/bin/node /usr/lib/node_modules/npm/bin/npm-cli.js "$@"\n' > /usr/bin/npm
    chmod 0755 /usr/bin/npm
  fi
}
if command -v npm >/dev/null 2>&1; then
  npm install --omit=dev
else
  echo "WARN: npm not wired; run: node /usr/lib/node_modules/npm/bin/npm-cli.js install --omit=dev (in $BASE/adapter)" >&2
fi

echo "==> rqlited (authority store, docs F8/F20)"
if ! command -v rqlited >/dev/null 2>&1; then
  RQ_VER=$(curl -fsSL https://api.github.com/repos/rqlite/rqlite/releases/latest | grep -oP '"tag_name"\s*:\s*"\K[^"]+')
  if [[ -z "${RQ_VER}" ]]; then echo "WARN: cannot resolve rqlite latest tag; install rqlited manually" >&2; else
    tmp=$(mktemp -d)
    curl -fsSL -o "$tmp/rqlite.tgz" "https://github.com/rqlite/rqlite/releases/download/${RQ_VER}/rqlite-${RQ_VER}-linux-amd64.tar.gz"
    tar -xzf "$tmp/rqlite.tgz" -C "$tmp"
    RQ_BIN=$(find "$tmp" -name rqlited -type f | head -1)
    install -m 0755 "$RQ_BIN" /usr/local/bin/rqlited
    rm -rf "$tmp"
  fi
fi
mkdir -p "$BASE/rqlite"
rqlited --version 2>/dev/null || true

echo "==> dedicated gateway user + relay group (docs 6.6.2 host defense)"
getent group relay >/dev/null || groupadd --system relay
id cliproxy >/dev/null 2>&1 || useradd --system --shell /usr/sbin/nologin --groups relay cliproxy
# gateway reads config + auths, writes logs; nothing else. The agent side (adapter,
# claude-config, workspace) stays root-owned — it is a different trust domain.
chown -R cliproxy:cliproxy "$BASE/logs"
chgrp cliproxy "$BASE/config.yaml" && chmod 0640 "$BASE/config.yaml"
chgrp -R cliproxy "$BASE/auths" && chmod 0750 "$BASE/auths" && find "$BASE/auths" -type f -exec chmod 0640 {} +

echo "==> systemd units"
cp -f "$(dirname "$0")/systemd/cli-proxy-api.service" /etc/systemd/system/
cp -f "$(dirname "$0")/systemd/relay-adapter@.service" /etc/systemd/system/
cp -f "$(dirname "$0")/systemd/rqlite-relay.service" /etc/systemd/system/
cp -f "$(dirname "$0")/systemd/relay-egress-guard.service" /etc/systemd/system/
install -m 0755 "$(dirname "$0")/egress-guard.sh" "$BASE/bin/egress-guard.sh"
systemctl daemon-reload
systemctl enable relay-egress-guard >/dev/null 2>&1 || true
systemctl start relay-egress-guard

echo "==> api key"
if grep -q "REPLACE_WITH_STRONG_API_KEY" "$BASE/config.yaml"; then
  KEY=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  sed -i "s/REPLACE_WITH_STRONG_API_KEY/${KEY}/" "$BASE/config.yaml"
  echo "    generated tenant api key: ${KEY}"
  echo "    (stored in $BASE/config.yaml — keep it secret)"
fi

cat <<'EOF'

==> next steps (operator, explicit):

1) Install the real Claude Code binary (glibc, never musl):
     curl -fsSL https://claude.ai/install.sh | bash     # installs to ~/.local/bin/claude
   or:  npm i -g @anthropic-ai/claude-code  (check the resolved binary is glibc!)
   Then copy it to a pinned location:
     install -m 0755 ~/.local/bin/claude /opt/cli-proxy/bin/claude

2) Seed ONE account's credentials (copy-once; docs 8.2 — never reseed a live account):
     CFG=/opt/cli-proxy/agents/claude-account.claude-config
     mkdir -p "$CFG"/{statsig,telemetry,sessions}
     # device_id: generate once, never rotate
     DEVICE_ID=$(openssl rand -hex 32)
     jq -n --arg uid "$DEVICE_ID" '{userID:$uid, installMethod:"native", autoUpdaterStatus:"enabled"}' > "$CFG/.claude.json"
     echo -n "acct-001" > "$CFG/.cc-sentinel"
     install -m 0600 /path/to/.credentials.json "$CFG/.credentials.json"   # from a logged-in claude

3) Agent env file /opt/cli-proxy/agents/claude-account.env
   (docs 6.7.1: per-account UDS, never TCP loopback on Linux):
     ADAPTER_LISTEN=unix:///run/relay/agent-claude-account.sock
     CLAUDE_BIN=/opt/cli-proxy/bin/claude
     PERSONA_CWD=/opt/cli-proxy/agents/workspace
     CLAUDE_CONFIG_DIR=/opt/cli-proxy/agents/claude-account.claude-config
   (and create the workspace dir)

4) The auth file for the gateway: auths/claude-account.json (provider "claude").
   Its filename stem must match the relay.agents key in config.yaml.

5) systemctl enable --now rqlite-relay cli-proxy-api relay-adapter@claude-account
   journalctl -u cli-proxy-api -f
   curl -s http://127.0.0.1:8317/healthz

WARNING (docs ch.6/7, read before seeding real credentials):
   This VPS is a DATACENTER IP. Serving a personal subscription account from a
   datacenter exit, with no microVM isolation and no residential proxy, is exactly
   the provenance-mismatch pattern the docs rate as a top ban driver. This install
   is a DEV/E2E harness. Do not point accounts you care about at it.
EOF

echo "==> setup done"
