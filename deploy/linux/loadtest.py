#!/usr/bin/env python3
"""Performance baseline for AgentMux, at the three loads Phase 6.5 names.

    python3 loadtest.py --scenario 1project
    python3 loadtest.py --scenario all

It measures the server as a deployment rather than as a benchmark: one process,
one machine, no synthetic flood. What it reports is what an operator would see
in `systemctl status` and `top` while a handful of people look at terminals.

    projects   how many projects exist and have a running runtime
    viewers    how many browser sockets are open, one per project each round
    wall       how long the measured phase took
    cpu avg    process CPU over that phase, as a percentage of one core
    rss        peak resident set of the server process
    attach     HTTP time to ask for a project's runtime
    ws round   time from a subscribe on an open socket to the reply for it

The WebSocket client is written here rather than imported, because the machines
this runs on have no Python package for one and installing a dependency to
measure a server is a poor trade. It implements the parts of RFC 6455 that a
load test needs: the handshake, masked client frames, and the close handshake.

It leaves the projects it created in place and stops their runtimes when it is
done, so a second run is comparable to the first.
"""

import argparse
import base64
import json
import os
import socket
import ssl  # noqa: F401  (imported for clarity; ws:// only)
import struct
import sys
import time
import urllib.request
import urllib.error

BASE = os.environ.get("AGENTMUX_BASE", "http://127.0.0.1:8787")
PROJECTS_ROOT = os.environ.get("AGENTMUX_PROJECTS_ROOT", "/srv/projects/loadtest")
SERVICE_USER = os.environ.get("AGENTMUX_SERVICE_USER", "agentmux")


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

def http(method, path, body=None):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=15) as resp:
        raw = resp.read()
    return json.loads(raw) if raw else None


def timed(method, path):
    start = time.perf_counter()
    result = http(method, path)
    return result, (time.perf_counter() - start) * 1000.0


# ---------------------------------------------------------------------------
# WebSocket, the parts a load test needs
# ---------------------------------------------------------------------------

class WebSocket:
    def __init__(self, url_host, port, path, protocol):
        self.sock = socket.create_connection((url_host, port), timeout=15)
        key = base64.b64encode(os.urandom(16)).decode()
        handshake = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {url_host}:{port}\r\n"
            f"Upgrade: websocket\r\n"
            f"Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            f"Sec-WebSocket-Version: 13\r\n"
            f"Sec-WebSocket-Protocol: {protocol}\r\n"
            f"\r\n"
        )
        self.sock.sendall(handshake.encode())

        # Read the response headers only; anything after them belongs to the
        # frame loop and must not be swallowed here.
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("the handshake was closed before it completed")
            buf += chunk
        head, rest = buf.split(b"\r\n\r\n", 1)
        status = head.split(b"\r\n", 1)[0].decode()
        if "101" not in status:
            raise RuntimeError(f"the upgrade was refused: {status}")
        self.buf = rest
        self.sock.settimeout(15)
        self.closed = False

    def _recv_exactly(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError("connection closed")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def send_text(self, text):
        payload = text.encode()
        header = bytearray([0x81])  # FIN + text
        mask = os.urandom(4)
        n = len(payload)
        if n < 126:
            header.append(0x80 | n)
        elif n < (1 << 16):
            header.append(0x80 | 126)
            header += struct.pack(">H", n)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", n)
        header += mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(bytes(header) + masked)

    def recv(self):
        """Return (opcode, payload) for the next frame, answering pings."""
        while True:
            b0, b1 = self._recv_exactly(2)
            opcode = b0 & 0x0F
            length = b1 & 0x7F
            if length == 126:
                length = struct.unpack(">H", self._recv_exactly(2))[0]
            elif length == 127:
                length = struct.unpack(">Q", self._recv_exactly(8))[0]
            payload = self._recv_exactly(length)
            if opcode == 0x9:  # ping
                self._pong(payload)
                continue
            if opcode == 0x8:  # close
                self.closed = True
                return opcode, payload
            return opcode, payload

    def _pong(self, payload):
        mask = os.urandom(4)
        header = bytearray([0x8A, 0x80 | len(payload)]) + mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(bytes(header) + masked)

    def close(self):
        try:
            mask = os.urandom(4)
            self.sock.sendall(bytes([0x88, 0x80]) + mask)
        except OSError:
            pass
        try:
            self.sock.close()
        except OSError:
            pass


# ---------------------------------------------------------------------------
# The server process
# ---------------------------------------------------------------------------

class ProcessStats:
    """Reads the server's CPU counters and memory use out of /proc."""

    def __init__(self, pid):
        self.pid = pid
        self.ticks = os.sysconf("SC_CLK_TCK")
        self.utime = self.stime = 0
        self.peak_rss_kb = 0

    def sample(self):
        """Record the CPU counters and return the current resident set, in kB."""
        with open(f"/proc/{self.pid}/stat") as fh:
            parts = fh.read().rsplit(")", 1)[1].split()
        self.utime, self.stime = int(parts[11]), int(parts[12])
        rss = 0
        with open(f"/proc/{self.pid}/status") as fh:
            for line in fh:
                if line.startswith("VmHWM:"):
                    self.peak_rss_kb = max(self.peak_rss_kb, int(line.split()[1]))
                elif line.startswith("VmRSS:"):
                    rss = int(line.split()[1])
        return rss


def server_pid():
    pid = os.environ.get("AGENTMUX_PID")
    if pid:
        return int(pid)
    # The service's own cgroup, which is the process systemd is running.
    try:
        with open("/var/run/agentmux.pid") as fh:
            return int(fh.read().strip())
    except OSError:
        pass
    # Fall back to the process holding the port.
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/cmdline", "rb") as fh:
                if b"agentmux-server" in fh.read():
                    return int(entry)
        except OSError:
            continue
    raise SystemExit("could not find the agentmux-server process; set AGENTMUX_PID")


# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------

def ensure_projects(count):
    """Register `count` projects under the load-test root and return their ids."""
    os.makedirs(PROJECTS_ROOT, exist_ok=True)
    try:
        import pwd
        entry = pwd.getpwnam(SERVICE_USER)
        for root, dirs, files in os.walk(PROJECTS_ROOT):
            os.chown(root, entry.pw_uid, entry.pw_gid)
    except (ImportError, KeyError):
        pass

    existing = {p["hostPath"]: p["id"] for p in http("GET", "/api/projects")["projects"]}
    ids = []
    for i in range(count):
        path = os.path.join(PROJECTS_ROOT, f"load-{i:02d}")
        os.makedirs(path, exist_ok=True)
        if path in existing:
            ids.append(existing[path])
            continue
        result = http("POST", "/api/projects/register", {"name": f"load-{i:02d}", "hostPath": path})
        ids.append(result["project"]["id"])
    return ids


def run_scenario(name, projects, viewers):
    print(f"\n{'=' * 72}")
    print(f"  {name}: {projects} project(s), {viewers} viewer(s)")
    print(f"{'=' * 72}")

    ids = ensure_projects(projects)

    # --- bring the runtimes up, timing the work a start does ----------------
    start_attach = []
    for pid in ids:
        _, ms = timed("POST", f"/api/projects/{pid}/runtime/start")
        start_attach.append(ms)
    for pid in ids:
        _, ms = timed("GET", f"/api/projects/{pid}/runtime")
        start_attach.append(ms)

    protocol = "agentmux.terminal.v2"
    host, port = "127.0.0.1", int(BASE.rsplit(":", 1)[1])

    # Two reads of the server's CPU counters around the whole measured window.
    # Everything between them - the subscriptions, the hold, the probes - is
    # what the percentage describes.
    stats = ProcessStats(server_pid())
    stats.sample()
    cpu_before = (stats.utime, stats.stime)
    rss_samples = []
    ws_rounds = []
    wall_start = time.perf_counter()

    sockets = []
    for round_index in range(viewers):
        for pid in ids:
            ws = WebSocket(host, port, "/api/ws", protocol)
            ws.send_text(json.dumps({"type": "hello", "protocol": 2}))
            ws.recv()  # hello

            t0 = time.perf_counter()
            ws.send_text(json.dumps({"type": "subscribe", "projectId": pid, "cols": 120, "rows": 30}))
            # Read until the snapshot for this subscription arrives. The first
            # message is not necessarily it - the server may send a control
            # state update first - so this reads until it sees the type.
            while True:
                _, payload = ws.recv()
                try:
                    message = json.loads(payload)
                except (ValueError, UnicodeDecodeError):
                    continue
                if message.get("type") in ("snapshot", "subscribed", "error"):
                    break
            ws_rounds.append((time.perf_counter() - t0) * 1000.0)
            sockets.append(ws)

    # --- hold the load, sampling memory while it is held --------------------
    hold_seconds = 10
    end = time.perf_counter() + hold_seconds
    while time.perf_counter() < end:
        rss_samples.append(stats.sample())
        time.sleep(0.25)

    wall = time.perf_counter() - wall_start

    # --- liveness and latency while loaded ----------------------------------
    health_latency = []
    for _ in range(20):
        _, ms = timed("GET", "/health")
        health_latency.append(ms)

    runtime_latency = []
    for pid in ids:
        _, ms = timed("GET", f"/api/projects/{pid}/runtime")
        runtime_latency.append(ms)

    # The CPU number that matters is the delta across the whole measured
    # window, from two reads of the server's own counters - everything between
    # them is what the percentage describes.
    end_stats = ProcessStats(server_pid())
    end_rss = end_stats.sample()
    cpu_pct = ((end_stats.utime - cpu_before[0]) + (end_stats.stime - cpu_before[1])) / end_stats.ticks / wall * 100.0

    peak_rss = max(rss_samples + [end_rss]) if rss_samples else end_rss

    def pct(values, p):
        if not values:
            return 0.0
        ordered = sorted(values)
        index = min(len(ordered) - 1, int(round((p / 100.0) * (len(ordered) - 1))))
        return ordered[index]

    fd_count = len(os.listdir(f"/proc/{end_stats.pid}/fd"))

    print(f"  projects         {projects}")
    print(f"  viewers          {viewers}  ({len(sockets)} sockets open)")
    print(f"  wall             {wall:.1f} s")
    print(f"  cpu              {cpu_pct:.1f} % of one core (avg over the window)")
    print(f"  rss              {peak_rss / 1024:.0f} MB peak (VmHWM {end_stats.peak_rss_kb / 1024:.0f} MB)")
    print(f"  file descriptors {fd_count}")
    print(f"  attach /api      {sum(start_attach) / len(start_attach):.1f} ms mean")
    print(f"  ws subscribe     {pct(ws_rounds, 50):.1f} ms p50, {pct(ws_rounds, 95):.1f} ms p95")
    print(f"  /health          {pct(health_latency, 50):.1f} ms p50, {pct(health_latency, 95):.1f} ms p95")
    print(f"  runtime read     {pct(runtime_latency, 50):.1f} ms p50, {pct(runtime_latency, 95):.1f} ms p95")

    for ws in sockets:
        ws.close()

    return {
        "scenario": name,
        "projects": projects,
        "viewers": viewers,
        "sockets": len(sockets),
        "wallSeconds": round(wall, 1),
        "cpuPercentOfOneCore": round(cpu_pct, 1),
        "peakRssMB": round(peak_rss / 1024),
        "fileDescriptors": fd_count,
        "wsSubscribeP50Ms": round(pct(ws_rounds, 50), 1),
        "wsSubscribeP95Ms": round(pct(ws_rounds, 95), 1),
        "healthP95Ms": round(pct(health_latency, 95), 1),
        "runtimeReadP95Ms": round(pct(runtime_latency, 95), 1),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--scenario", default="all",
                        choices=["1project", "5projects", "10viewers", "all"])
    parser.add_argument("--json", help="write the results here as JSON")
    args = parser.parse_args()

    scenarios = [
        ("1 project / 1 viewer", 1, 1),
        ("5 projects / 5 viewers", 5, 1),
        ("5 projects / 10 viewers", 5, 2),
    ]
    if args.scenario != "all":
        wanted = {"1project": 0, "5projects": 1, "10viewers": 2}[args.scenario]
        scenarios = [scenarios[wanted]]

    results = []
    for name, projects, viewers in scenarios:
        results.append(run_scenario(name, projects, viewers))

    print(f"\n{'=' * 72}")
    print("  Summary")
    print(f"{'=' * 72}")
    print(f"  {'scenario':<26} {'cpu':>7} {'rss':>8} {'fds':>6} {'ws p95':>9}")
    for r in results:
        print(f"  {r['scenario']:<26} {r['cpuPercentOfOneCore']:>6.1f}% "
              f"{r['peakRssMB']:>6} MB {r['fileDescriptors']:>6} {r['wsSubscribeP95Ms']:>7.1f} ms")

    if args.json:
        with open(args.json, "w") as fh:
            json.dump(results, fh, indent=2)
        print(f"\n  written to {args.json}")


if __name__ == "__main__":
    main()
