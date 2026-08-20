#!/usr/bin/env python3
"""Loopback CONNECT proxy that egresses directly to an allow-listed host set.

Replaces the dumb TCP relay that forwarded to isp.oxylabs.io. That upstream now
answers 407 for every credential we hold, which surfaced at the gateway as
"exchange_user_api_key: Unauthorized" and looked like a Cursor auth failure.

The gateway (uid cliproxy) is still kernel-blocked from all non-loopback egress
by relay-egress-guard, so it keeps talking only to 127.0.0.1 and this process
does the reaching out. The host allow-list preserves the reason that guard
exists: Anthropic stays unreachable from the gateway, now enforced by name
rather than only by uid.

Long Cursor Agent runs stay quiet for minutes while the model thinks, so the
idle handling and TCP keepalive of the original relay are kept verbatim: an
idle tunnel is never closed here, and a dead peer is reaped by keepalive.
"""

from __future__ import annotations

import argparse
import select
import socket
import socketserver
import sys

BUFFER_SIZE = 64 * 1024
CONNECT_TIMEOUT = 15
# Must exceed Claude Code / Cursor long-think turns (often 10-15 minutes).
IDLE_TIMEOUT = 1800
REQUEST_HEAD_LIMIT = 16 * 1024

DEFAULT_ALLOW = ("api2.cursor.sh", ".cursor.sh", "cursor.com", ".cursor.com")


def _enable_keepalive(sock: socket.socket) -> None:
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
    if hasattr(socket, "TCP_KEEPIDLE"):
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPIDLE, 60)
    if hasattr(socket, "TCP_KEEPINTVL"):
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPINTVL, 30)
    if hasattr(socket, "TCP_KEEPCNT"):
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPCNT, 8)


def host_allowed(host: str, allow: tuple[str, ...]) -> bool:
    h = host.strip().lower().rstrip(".")
    for entry in allow:
        e = entry.lower()
        if e.startswith("."):
            if h.endswith(e):
                return True
        elif h == e:
            return True
    return False


class ConnectHandler(socketserver.BaseRequestHandler):
    def _reply(self, status: str) -> None:
        try:
            self.request.sendall(f"HTTP/1.1 {status}\r\nConnection: close\r\n\r\n".encode())
        except OSError:
            pass

    def _read_head(self) -> bytes | None:
        buf = b""
        self.request.settimeout(CONNECT_TIMEOUT)
        while b"\r\n\r\n" not in buf:
            try:
                chunk = self.request.recv(BUFFER_SIZE)
            except OSError:
                return None
            if not chunk:
                return None
            buf += chunk
            if len(buf) > REQUEST_HEAD_LIMIT:
                return None
        return buf

    def handle(self) -> None:
        head = self._read_head()
        if not head:
            return
        request_line = head.split(b"\r\n", 1)[0].decode("latin-1", "replace")
        parts = request_line.split()
        if len(parts) < 2 or parts[0].upper() != "CONNECT":
            self._reply("405 Method Not Allowed")
            return

        target = parts[1]
        host, _, port_s = target.rpartition(":")
        if not host:
            host, port_s = target, "443"
        try:
            port = int(port_s)
        except ValueError:
            self._reply("400 Bad Request")
            return

        if not host_allowed(host, self.server.allow):
            print(f"deny CONNECT {host}:{port}", flush=True)
            self._reply("403 Forbidden")
            return
        if port not in self.server.allow_ports:
            print(f"deny CONNECT {host}:{port} (port)", flush=True)
            self._reply("403 Forbidden")
            return

        try:
            upstream = socket.create_connection((host, port), timeout=CONNECT_TIMEOUT)
        except OSError as e:
            print(f"upstream {host}:{port} failed: {e}", flush=True)
            self._reply("502 Bad Gateway")
            return

        try:
            self._reply("200 Connection Established")
            self.request.settimeout(None)
            _enable_keepalive(self.request)
            _enable_keepalive(upstream)
            self.request.setblocking(False)
            upstream.setblocking(False)
            peers = (self.request, upstream)
            idle = self.server.idle_timeout
            while True:
                readable, _, exceptional = select.select(peers, (), peers, idle)
                if exceptional:
                    return
                if not readable:
                    # Idle on both sides. Keep waiting; TCP keepalive will
                    # reap a dead peer. Closing here used to cut long thinks.
                    continue
                for source in readable:
                    try:
                        data = source.recv(BUFFER_SIZE)
                    except BlockingIOError:
                        continue
                    if not data:
                        return
                    target_sock = upstream if source is self.request else self.request
                    try:
                        _sendall_blocking(target_sock, data)
                    except OSError:
                        return
        finally:
            upstream.close()


def _sendall_blocking(sock: socket.socket, data: bytes) -> None:
    """sendall() on a non-blocking socket raises EAGAIN once the peer stalls.

    The old relay called sendall() directly and died with BlockingIOError
    mid-turn; wait for writability instead of dropping the tunnel.
    """
    view = memoryview(data)
    while view:
        try:
            sent = sock.send(view)
            view = view[sent:]
        except BlockingIOError:
            _, writable, _ = select.select((), (sock,), (), 60)
            if not writable:
                raise OSError("send stalled")


class ConnectProxy(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, address, allow, allow_ports, idle_timeout=IDLE_TIMEOUT):
        self.allow = allow
        self.allow_ports = allow_ports
        self.idle_timeout = idle_timeout
        super().__init__(address, ConnectHandler)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument(
        "--allow",
        default=",".join(DEFAULT_ALLOW),
        help="comma-separated hosts; a leading dot matches subdomains",
    )
    parser.add_argument("--allow-ports", default="443")
    parser.add_argument("--idle-timeout", type=float, default=IDLE_TIMEOUT)
    args = parser.parse_args()

    if args.listen not in ("127.0.0.1", "::1", "localhost"):
        print("refusing to bind a non-loopback address", file=sys.stderr)
        raise SystemExit(2)

    allow = tuple(h.strip() for h in args.allow.split(",") if h.strip())
    allow_ports = {int(p) for p in args.allow_ports.split(",") if p.strip()}
    print(f"cursor-connect-proxy on {args.listen}:{args.port} allow={allow} ports={sorted(allow_ports)}",
          flush=True)

    with ConnectProxy((args.listen, args.port), allow, allow_ports, args.idle_timeout) as server:
        server.serve_forever(poll_interval=0.5)


if __name__ == "__main__":
    main()
