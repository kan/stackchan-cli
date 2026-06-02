"""
stdio MCP smoke test for stackchan-mcp gateway (no device required).

Spawns `stackchan-mcp` and drives the MCP stdio transport (newline-delimited
JSON-RPC) through: initialize -> notifications/initialized -> tools/list ->
tools/call get_status. This exercises exactly the surface our Go CLI (Plan B)
will talk to, and captures the static tools/list response.

Run:  py scripts/smoke_stdio.py
Needs: STACKCHAN_TOKEN (any value works for the stdio side).
"""
import json
import os
import subprocess
import sys
import threading
import time

EXE = os.environ.get("STACKCHAN_MCP_EXE", os.path.expanduser(r"~\.local\bin\stackchan-mcp.exe"))


def main():
    env = dict(os.environ)
    env.setdefault("STACKCHAN_TOKEN", "smoke-test-token")

    proc = subprocess.Popen(
        [EXE],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        text=True,
        encoding="utf-8",
        errors="replace",
        bufsize=1,
    )

    out_lines = []
    err_lines = []

    def drain(stream, sink):
        for line in iter(stream.readline, ""):
            sink.append(line.rstrip("\n"))

    t_out = threading.Thread(target=drain, args=(proc.stdout, out_lines), daemon=True)
    t_err = threading.Thread(target=drain, args=(proc.stderr, err_lines), daemon=True)
    t_out.start()
    t_err.start()

    def send(obj):
        if proc.poll() is not None:
            print("[send skipped: process exited code=%s]" % proc.returncode)
            return
        try:
            proc.stdin.write(json.dumps(obj) + "\n")
            proc.stdin.flush()
        except OSError as e:
            print("[send failed: %s; process exit code=%s]" % (e, proc.poll()))

    # 1) initialize
    send({
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "smoke", "version": "0.0.1"},
        },
    })
    time.sleep(1.5)
    # 2) initialized notification
    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    time.sleep(0.3)
    # 3) tools/list
    send({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
    time.sleep(1.0)
    # 4) tools/call get_status (handled locally, no device needed)
    send({
        "jsonrpc": "2.0", "id": 3, "method": "tools/call",
        "params": {"name": "get_status", "arguments": {}},
    })
    time.sleep(1.5)

    try:
        proc.terminate()
        proc.wait(timeout=5)
    except Exception:
        proc.kill()

    print("[process exit code=%s]" % proc.returncode)

    print("===== STDOUT (JSON-RPC) =====")
    for ln in out_lines:
        print(ln)
    print("\n===== STDERR (logs) =====")
    for ln in err_lines:
        print(ln)


if __name__ == "__main__":
    main()
