"""
Dump the gateway's static tools/list to docs/gateway-tools.json (pretty JSON).
Reuses the stdio handshake from smoke_stdio. No device required.

Run:  py scripts/dump_tools.py
"""
import json
import os
import subprocess
import sys
import threading
import time

EXE = os.environ.get("STACKCHAN_MCP_EXE", os.path.expanduser(r"~\.local\bin\stackchan-mcp.exe"))
OUT = os.path.join(os.path.dirname(__file__), "..", "docs", "gateway-tools.json")


def main():
    env = dict(os.environ)
    env.setdefault("STACKCHAN_TOKEN", "smoke-test-token")
    proc = subprocess.Popen(
        [EXE], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL, env=env, text=True,
        encoding="utf-8", errors="replace", bufsize=1,
    )

    out_lines = []

    def drain():
        for line in iter(proc.stdout.readline, ""):
            out_lines.append(line.rstrip("\n"))

    threading.Thread(target=drain, daemon=True).start()

    def send(obj):
        proc.stdin.write(json.dumps(obj) + "\n")
        proc.stdin.flush()

    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                     "clientInfo": {"name": "dump", "version": "0.0.1"}}})
    time.sleep(1.5)
    send({"jsonrpc": "2.0", "method": "notifications/initialized"})
    time.sleep(0.3)
    send({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
    time.sleep(1.5)
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except Exception:
        proc.kill()

    tools_resp = None
    for ln in out_lines:
        try:
            obj = json.loads(ln)
        except ValueError:
            continue
        if obj.get("id") == 2 and "result" in obj:
            tools_resp = obj["result"]
            break

    if tools_resp is None:
        print("ERROR: no tools/list response captured", file=sys.stderr)
        sys.exit(1)

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as f:
        json.dump(tools_resp, f, ensure_ascii=False, indent=2)
    tools = tools_resp.get("tools", [])
    print("wrote %d tools to %s" % (len(tools), os.path.normpath(OUT)))
    for t in tools:
        req = t.get("inputSchema", {}).get("required", [])
        print("  - %-22s req=%s" % (t["name"], req))


if __name__ == "__main__":
    main()
