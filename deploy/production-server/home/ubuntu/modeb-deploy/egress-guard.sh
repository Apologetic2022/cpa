#!/usr/bin/env bash
# Docs 6.6.2 host-side structural defense: the gateway process (dedicated "cliproxy"
# user) must NEVER open a non-loopback socket — stronger than "DROP gateway->Anthropic":
# every non-loopback egress from the gateway uid is dropped by the kernel, so even a
# code regression or a malicious dependency cannot exfiltrate or touch Anthropic.
# Loopback stays open (tenant API on 127.0.0.1:8317, rqlited on 127.0.0.1:4001) and the
# per-account UDS data plane is not network traffic at all.
# Idempotent; safe to run at every boot.
set -euo pipefail

GUARD_USER=cliproxy

uid=$(id -u "$GUARD_USER" 2>/dev/null) || {
  echo "egress-guard: user $GUARD_USER missing" >&2
  exit 1
}

add_rule() {
  local tool=$1
  shift
  if ! "$tool" -C OUTPUT "$@" 2>/dev/null; then
    "$tool" -A OUTPUT "$@"
  fi
}

# IPv4 + IPv6: drop anything from the gateway uid that is not leaving via lo.
add_rule iptables  -m owner --uid-owner "$uid" ! -o lo -j DROP
add_rule ip6tables -m owner --uid-owner "$uid" ! -o lo -j DROP

echo "egress-guard: uid $uid ($GUARD_USER) non-loopback egress dropped (v4+v6)"
