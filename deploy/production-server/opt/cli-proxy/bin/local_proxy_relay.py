#!/usr/bin/env python3
"""Small loopback-only TCP relay for a local service using an upstream proxy.

Long Cursor Agent runs stay quiet for minutes while the model thinks. A short
idle close looks like "API Error: Server error mid-response" at the client
after a clean 5-10 minute cogitation. Keep the tunnel open for the whole turn
and use TCP keepalive so a dead peer is still detected.
"""

from __future__ import annotations

import argparse
import select
import socket
import socketserver


BUFFER_SIZE = 64 * 1024
CONNECT_TIMEOUT = 15
# Must exceed Claude Code / Cursor long-think turns (often 10-15 minutes).
IDLE_TIMEOUT = 1800


def _enable_keepalive(sock: socket.socket) -> None:
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
    if hasattr(socket, "TCP_KEEPIDLE"):
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPIDLE, 60)
    if hasattr(socket, "TCP_KEEPINTVL"):
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPINTVL, 30)
    if hasattr(socket, "TCP_KEEPCNT"):
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_KEEPCNT, 8)


class RelayHandler(socketserver.BaseRequestHandler):
    def handle(self) -> None:
        upstream = socket.create_connection(
            (self.server.upstream_host, self.server.upstream_port),
            timeout=CONNECT_TIMEOUT,
        )
        try:
            _enable_keepalive(self.request)
            _enable_keepalive(upstream)
            self.request.setblocking(False)
            upstream.setblocking(False)
            peers = (self.request, upstream)
            idle = self.server.idle_timeout
            while True:
                readable, _, exceptional = select.select(
                    peers, (), peers, idle
                )
                if exceptional:
                    return
                if not readable:
                    # Idle on both sides. Keep waiting; TCP keepalive will
                    # reap a dead peer. Closing here used to cut long thinks.
                    continue
                for source in readable:
                    data = source.recv(BUFFER_SIZE)
                    if not data:
                        return
                    target = upstream if source is self.request else self.request
                    target.sendall(data)
        finally:
            upstream.close()


class RelayServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(
        self,
        address: tuple[str, int],
        upstream: tuple[str, int],
        idle_timeout: float = IDLE_TIMEOUT,
    ):
        self.upstream_host, self.upstream_port = upstream
        self.idle_timeout = idle_timeout
        super().__init__(address, RelayHandler)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--upstream", required=True)
    parser.add_argument("--upstream-port", type=int, required=True)
    parser.add_argument(
        "--idle-timeout",
        type=float,
        default=IDLE_TIMEOUT,
        help="select() wait in seconds; idle no longer closes the tunnel",
    )
    args = parser.parse_args()

    with RelayServer(
        (args.listen, args.port),
        (args.upstream, args.upstream_port),
        idle_timeout=args.idle_timeout,
    ) as server:
        server.serve_forever(poll_interval=0.5)


if __name__ == "__main__":
    main()
