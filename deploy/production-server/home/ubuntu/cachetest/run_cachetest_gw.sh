#!/bin/bash
# Start the isolated cache-test gateway on the production host (port 8318).
# Usage: run_cachetest_gw.sh <binary-name>
set -u
BIN="${1:-cpa-baseline}"
PROXY='http://REDACTED_PROXY_USER:REDACTED_PROXY_PASS@127.0.0.1:18006'
pkill -f 'cachetest/cpa-' 2>/dev/null
sleep 1
cd "$HOME/cachetest" || exit 1
CURSOR_DISABLE_CONV_REUSE="${CURSOR_DISABLE_CONV_REUSE:-}" \
  HTTPS_PROXY="$PROXY" HTTP_PROXY="$PROXY" https_proxy="$PROXY" http_proxy="$PROXY" \
  NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost \
  nohup "./$BIN" -config "$HOME/cachetest/config.yaml" >"$HOME/cachetest/$BIN.out" 2>&1 &
sleep 5
echo "started $BIN pid=$!"
tail -5 "$HOME/cachetest/$BIN.out"
