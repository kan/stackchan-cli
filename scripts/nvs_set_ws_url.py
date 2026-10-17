"""Rewrite the StackChan's gateway URL (NVS websocket.url) over USB, keeping Wi-Fi settings.

Fallback for when the device cannot reach the gateway (e.g. the PC's LAN IP
changed) and the captive portal is hard to enter. When the device is
connected, prefer the MCP way instead:

    stackchan-cli call gateway_config_set --json '{"url":"ws://<PC-IP>:8765/"}'

What this does (stdlib only; needs `esptool` on PATH, v5+):
  1. esptool read-flash 0x8000..0xD000 (partition table + NVS) -> backup file
  2. find the `websocket.url` string entry, replace its value in place and
     recompute the entry's data CRC and header CRC (NVS format)
  3. esptool write-flash the NVS partition back, then hard-reset

Only a URL that is not longer than the current one fits in place (shorter
ones are NUL-padded, which nvs_get_str readers ignore).

usage:
  py scripts/nvs_set_ws_url.py --port COM3 --url ws://<PC-IP>:8765/
  py scripts/nvs_set_ws_url.py --port COM3 --show          # read & print only
  py scripts/nvs_set_ws_url.py --port COM3 --url ... --dry-run
"""
import argparse
import collections
import datetime
import os
import struct
import subprocess
import sys
import tempfile
import zlib

PT_OFFSET = 0x8000
DUMP_SIZE = 0x5000  # partition table + 16 KB NVS on the stackchan layouts
PAGE = 4096
ENTRY = 32
TYPE_U8 = 0x01
TYPE_STR = 0x21


def crc(data):
    return zlib.crc32(bytes(data), 0xFFFFFFFF) & 0xFFFFFFFF


def esptool(port, *args):
    cmd = ["esptool", "--chip", "esp32s3", "--port", port,
           "--before", "default-reset", "--after", "hard-reset", *args]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        sys.exit(f"esptool failed ({' '.join(args[:1])}):\n{res.stdout[-2000:]}{res.stderr[-2000:]}")


def find_nvs(dump):
    for i in range(0, 0xC00, 32):
        e = dump[i:i + 32]
        if e[:2] == b"\xaa\x50" and e[12:28].split(b"\0")[0] == b"nvs":
            return struct.unpack_from("<II", e, 4)
    sys.exit("nvs partition not found in the partition table")


StrEntry = collections.namedtuple("StrEntry", "off ns key size value hdr_ok data_ok")


def written_entries(nvs):
    """Yield (offset_in_nvs, first 32-byte entry) for every written entry."""
    for p in range(len(nvs) // PAGE):
        base = p * PAGE
        if struct.unpack_from("<I", nvs, base)[0] == 0xFFFFFFFF:
            continue  # uninitialized page
        i = 0
        while i < 126:
            state = (nvs[base + 32 + i // 4] >> ((i % 4) * 2)) & 3
            if state != 2:  # not "written"
                i += 1
                continue
            off = base + 64 + i * ENTRY
            e = nvs[off:off + ENTRY]
            yield off, e
            i += e[2] or 1


def string_entries(nvs):
    """Yield a StrEntry for every written string entry (off = offset in nvs).

    Namespace names are collected first: a namespace's index entry can sit on
    a page after the entries that use it.
    """
    namespaces = {e[24]: e[8:24].split(b"\0")[0].decode(errors="replace")
                  for _, e in written_entries(nvs) if e[0] == 0 and e[1] == TYPE_U8}
    for off, e in written_entries(nvs):
        if e[0] == 0 or e[1] != TYPE_STR:
            continue
        key = e[8:24].split(b"\0")[0].decode(errors="replace")
        hdr_ok = struct.unpack_from("<I", e, 4)[0] == crc(e[0:4] + e[8:32])
        size, _, dcrc = struct.unpack_from("<HHI", e, 24)
        val = nvs[off + ENTRY:off + ENTRY + size]
        yield StrEntry(off, namespaces.get(e[0], f"#{e[0]}"), key, size, val, hdr_ok, dcrc == crc(val))


def ws_url_entries(nvs):
    return [e for e in string_entries(nvs) if e.ns == "websocket" and e.key == "url"]


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--port", required=True, help="serial port, e.g. COM3")
    ap.add_argument("--url", help="new websocket.url, e.g. ws://<PC-IP>:8765/")
    ap.add_argument("--show", action="store_true", help="only print the current URL")
    ap.add_argument("--dry-run", action="store_true", help="patch the dump but do not write the device")
    ap.add_argument("--backup-dir", default=os.path.join(os.path.expanduser("~"), ".stackchan", "nvs-backup"))
    args = ap.parse_args()
    if not args.show and not args.url:
        ap.error("--url or --show is required")

    os.makedirs(args.backup_dir, exist_ok=True)
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    dump_path = os.path.join(args.backup_dir, f"pt_nvs-{stamp}.bin")
    esptool(args.port, "read-flash", hex(PT_OFFSET), hex(DUMP_SIZE), dump_path)
    dump = bytearray(open(dump_path, "rb").read())
    print(f"backup: {dump_path} (flash 0x8000..0x{PT_OFFSET + DUMP_SIZE:x})")

    nvs_off, nvs_size = find_nvs(dump)
    start = nvs_off - PT_OFFSET
    if start + nvs_size > len(dump):
        sys.exit(f"nvs partition 0x{nvs_off:x}+0x{nvs_size:x} is outside the dump")
    nvs = dump[start:start + nvs_size]

    if not all(e.hdr_ok and e.data_ok for e in string_entries(nvs)):
        sys.exit("CRC mismatch in existing entries; refusing to patch (parser/format mismatch?)")
    urls = ws_url_entries(nvs)
    if len(urls) != 1:
        sys.exit(f"expected exactly one websocket.url entry, found {len(urls)}")
    off, size = urls[0].off, urls[0].size
    current = urls[0].value.split(b"\0")[0].decode()
    print(f"current websocket.url: {current!r}")
    if args.show:
        return

    new = args.url.encode()
    if len(new) + 1 > size:
        sys.exit(f"new URL is {len(new)} bytes; at most {size - 1} fit in place "
                 "(use the captive portal or a full reflash instead)")
    padded = new + b"\0" * (size - len(new))
    nvs[off + ENTRY:off + ENTRY + size] = padded
    struct.pack_into("<I", nvs, off + 28, crc(padded))
    hdr = nvs[off:off + ENTRY]
    struct.pack_into("<I", nvs, off + 4, crc(hdr[0:4] + hdr[8:32]))

    check = ws_url_entries(nvs)[0]
    if not (check.hdr_ok and check.data_ok) or check.value.split(b"\0")[0] != new:
        sys.exit("self-check of the patched entry failed; nothing written")
    print(f"new websocket.url:     {args.url!r}")

    if args.dry_run:
        print("dry run: device not written")
        return
    fd, patched = tempfile.mkstemp(suffix=".bin")
    with os.fdopen(fd, "wb") as f:
        f.write(nvs)
    try:
        esptool(args.port, "write-flash", hex(nvs_off), patched)
    finally:
        os.remove(patched)
    print(f"written to 0x{nvs_off:x}; the device is rebooting. Restore with:\n"
          f"  esptool --chip esp32s3 --port {args.port} write-flash 0x8000 {dump_path}")


if __name__ == "__main__":
    main()
