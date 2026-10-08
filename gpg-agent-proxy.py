#!/usr/bin/env python3
# -*- coding: utf-8 -*-
r"""
gpg-agent-proxy - let OpenSSH forward the Windows gpg-agent.

Why this exists
---------------
On Windows, GnuPG 2.5's gpg-agent does not listen on a filesystem socket.
It listens on a random TCP port on 127.0.0.1 and requires every client to
send a 16-byte nonce right after connecting.  Port number and nonce live in
    %LOCALAPPDATA%\gnupg\S.gpg-agent.extra
(first line = port, then 16 raw nonce bytes).

So OpenSSH's RemoteForward cannot point at it directly.  This proxy listens
on a plain TCP port on 127.0.0.1 and, for every incoming connection, dials
the real agent, replays the nonce, then shovels bytes both ways.

Matching ssh config:
    RemoteForward /run/user/<uid>/gnupg/S.gpg-agent 127.0.0.1:16688
    StreamLocalBindUnlink yes

Stop with Ctrl+C (SIGINT / SIGBREAK).
"""

import os
import signal
import socket
import sys
import threading
import time

SOCK_FILE = os.path.join(os.environ.get("LOCALAPPDATA", ""), "gnupg", "S.gpg-agent.extra")
LISTEN_HOST = "127.0.0.1"
LISTEN_PORT = int(os.environ.get("GPG_PROXY_PORT", "16688"))
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
LOG_FILE = os.path.join(SCRIPT_DIR, "gpg-agent-proxy.log")

_stop = threading.Event()
_active = set()
_active_lock = threading.Lock()
_handlers = []


def log(msg):
    """Print to the console and append to the log file."""
    line = "%s %s" % (time.strftime("%H:%M:%S"), msg)
    try:
        with open(LOG_FILE, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
    except OSError:
        pass
    try:
        print(line, flush=True)
    except (OSError, ValueError):
        pass


def read_endpoint():
    """Read port + nonce.  Done per connection so an agent restart is picked up."""
    with open(SOCK_FILE, "rb") as fh:
        data = fh.read()
    pos = data.find(b"\n")
    if pos <= 0:
        raise RuntimeError("unexpected socket file content: %r" % data[:32])
    port = int(data[:pos])
    nonce = data[pos + 1:pos + 17]
    if len(nonce) != 16:
        raise RuntimeError("unexpected nonce length: %d" % len(nonce))
    return port, nonce


def track(sock, add):
    with _active_lock:
        if add:
            _active.add(sock)
        else:
            _active.discard(sock)


def close_sock(sock):
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    try:
        sock.close()
    except OSError:
        pass
    track(sock, False)


def pump(src, dst, stats, key):
    try:
        while not _stop.is_set():
            chunk = src.recv(65536)
            if not chunk:
                break
            stats[key] += len(chunk)
            dst.sendall(chunk)
    except OSError:
        pass
    finally:
        # half-close both ends so the opposite pump wakes up too
        for sock in (src, dst):
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def handle(client):
    track(client, True)
    upstream = None
    stats = {"up": 0, "down": 0}
    try:
        port, nonce = read_endpoint()
        upstream = socket.create_connection((LISTEN_HOST, port), timeout=10)
        upstream.sendall(nonce)
        upstream.settimeout(None)
        client.settimeout(None)
        track(upstream, True)
        log("connect -> 127.0.0.1:%d" % port)
        mate = threading.Thread(target=pump, args=(upstream, client, stats, "down"), daemon=True)
        mate.start()
        pump(client, upstream, stats, "up")
        mate.join(timeout=1)
        log("closed  up=%dB down=%dB" % (stats["up"], stats["down"]))
    except Exception as exc:  # noqa: BLE001 - never let a bad client kill the proxy
        if not _stop.is_set():
            log("error: %s: %s" % (type(exc).__name__, exc))
    finally:
        if upstream is not None:
            close_sock(upstream)
        close_sock(client)


def shutdown_all():
    with _active_lock:
        socks = list(_active)
    for sock in socks:
        close_sock(sock)


def _on_signal(signum, _frame):
    if _stop.is_set():          # a second Ctrl+C while winding down: just ignore it
        return
    _stop.set()
    raise KeyboardInterrupt()


def serve(srv):
    srv.listen(32)
    srv.settimeout(1.0)         # keeps the accept loop responsive to Ctrl+C
    log("gpg-agent-proxy listening on %s:%d" % (LISTEN_HOST, LISTEN_PORT))
    log("upstream agent endpoint: %s" % SOCK_FILE)
    log("press Ctrl+C to stop")
    while not _stop.is_set():
        client = None
        try:
            client, _addr = srv.accept()
        except socket.timeout:
            pass
        except KeyboardInterrupt:
            raise
        except OSError as exc:
            if not _stop.is_set():
                log("accept failed: %s" % exc)
            break
        if _stop.is_set():                  # Ctrl+C raced with a new connection
            if client is not None:
                close_sock(client)
            break
        if client is None:
            continue
        worker = threading.Thread(target=handle, args=(client,), daemon=True)
        _handlers.append(worker)
        worker.start()


def shutdown(srv):
    _stop.set()
    log("shutting down ...")
    try:
        srv.close()
    except OSError:
        pass
    shutdown_all()
    for worker in list(_handlers):          # let active tunnels unwind before we exit
        worker.join(timeout=1.5)
    log("stopped")


def main():
    for name in ("SIGINT", "SIGBREAK", "SIGTERM"):
        sig = getattr(signal, name, None)
        if sig is None:
            continue
        try:
            signal.signal(sig, _on_signal)
        except (ValueError, OSError):
            pass

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    if os.name == "nt":
        # On Windows SO_REUSEADDR would let a second instance silently share the
        # port; SO_EXCLUSIVEADDRUSE makes a duplicate start fail loudly instead.
        if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
            srv.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
    else:
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        srv.bind((LISTEN_HOST, LISTEN_PORT))
    except OSError as exc:
        log("cannot bind %s:%d (%s)" % (LISTEN_HOST, LISTEN_PORT, exc))
        log("already running? see: netstat -ano | findstr :%d" % LISTEN_PORT)
        return 1

    try:
        serve(srv)
    except KeyboardInterrupt:
        pass
    finally:
        try:
            shutdown(srv)
        except KeyboardInterrupt:           # never let a stray Ctrl+C abort the cleanup
            pass
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:               # belt and braces: no traceback, exit 0
        sys.exit(0)
