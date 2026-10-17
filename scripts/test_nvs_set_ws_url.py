"""Tests for nvs_set_ws_url.py on synthetic NVS images (no device needed).

run: python -m unittest discover -s scripts -p "test_*.py"
"""
import struct
import unittest

import nvs_set_ws_url as nvs_tool
from nvs_set_ws_url import ENTRY, PAGE, TYPE_STR, TYPE_U8, crc

STATE_WRITTEN = 2
STATE_ERASED = 0
OLD_URL = b"ws://192.168.1.70:8765/"


class NvsImage:
    """Builds an NVS partition image entry by entry, in the ESP-IDF layout:
    32-byte page header, 32-byte entry state bitmap (2 bits per entry),
    then 126 entries of 32 bytes."""

    def __init__(self, pages=4):
        self.data = bytearray(b"\xff" * (PAGE * pages))
        self.next_index = {}

    def _put(self, page, entries, state=STATE_WRITTEN):
        """Write entries (a multiple of 32 bytes) at the page's next free slot."""
        base = page * PAGE
        struct.pack_into("<I", self.data, base, 0xFFFFFFFE)  # page state: active
        first = self.next_index.get(page, 0)
        span = len(entries) // ENTRY
        off = base + 64 + first * ENTRY
        self.data[off:off + len(entries)] = entries
        for i in range(first, first + span):
            pos = base + 32 + i // 4
            shift = (i % 4) * 2
            self.data[pos] = (self.data[pos] & ~(3 << shift) & 0xFF) | (state << shift)
        self.next_index[page] = first + span
        return off

    @staticmethod
    def _header(ns, typ, span, key, data8):
        e = bytearray(ENTRY)
        e[0], e[1], e[2], e[3] = ns, typ, span, 0xFF
        e[8:24] = key.encode().ljust(16, b"\0")
        e[24:32] = data8
        struct.pack_into("<I", e, 4, crc(e[0:4] + e[8:32]))
        return e

    def namespace(self, page, name, index):
        data8 = bytes([index]) + b"\xff" * 7
        return self._put(page, self._header(0, TYPE_U8, 1, name, data8))

    def string(self, page, ns_index, key, value, state=STATE_WRITTEN):
        """value includes the terminating NUL, as nvs_set_str stores it."""
        span = 1 + (len(value) + ENTRY - 1) // ENTRY
        data8 = struct.pack("<HHI", len(value), 0xFFFF, crc(value))
        body = value.ljust((span - 1) * ENTRY, b"\xff")
        return self._put(page, self._header(ns_index, TYPE_STR, span, key, data8) + body, state)


def device_image():
    """An image like the device's: Wi-Fi settings and the gateway URL."""
    img = NvsImage()
    img.namespace(0, "wifi", 1)
    img.string(0, 1, "ssid", b"home-ap\0")
    img.namespace(0, "websocket", 2)
    img.string(0, 2, "url", OLD_URL + b"\0")
    img.string(0, 2, "token", b"\0")
    return img


class ParseTest(unittest.TestCase):
    def test_entries_and_crcs(self):
        entries = list(nvs_tool.string_entries(device_image().data))
        self.assertEqual([(e.ns, e.key) for e in entries],
                         [("wifi", "ssid"), ("websocket", "url"), ("websocket", "token")])
        self.assertTrue(all(e.hdr_ok and e.data_ok for e in entries))
        _, url = nvs_tool.current_ws_url(device_image().data)
        self.assertEqual(url, OLD_URL.decode())

    def test_namespace_on_a_later_page(self):
        img = NvsImage()
        img.string(0, 3, "url", OLD_URL + b"\0")
        img.namespace(2, "websocket", 3)
        _, url = nvs_tool.current_ws_url(img.data)
        self.assertEqual(url, OLD_URL.decode())

    def test_erased_entry_is_skipped(self):
        img = NvsImage()
        img.namespace(0, "websocket", 2)
        img.string(0, 2, "url", b"ws://10.0.0.1:8765/\0", state=STATE_ERASED)
        img.string(0, 2, "url", OLD_URL + b"\0")
        _, url = nvs_tool.current_ws_url(img.data)
        self.assertEqual(url, OLD_URL.decode())

    def test_crc_mismatch_is_refused(self):
        img = device_image()
        entry, _ = nvs_tool.current_ws_url(img.data)
        img.data[entry.off + ENTRY] ^= 0x01  # corrupt the URL value
        with self.assertRaisesRegex(ValueError, "CRC mismatch"):
            nvs_tool.current_ws_url(img.data)

    def test_missing_or_duplicate_url(self):
        img = NvsImage()
        img.namespace(0, "websocket", 2)
        with self.assertRaisesRegex(ValueError, "found 0"):
            nvs_tool.current_ws_url(img.data)
        img.string(0, 2, "url", OLD_URL + b"\0")
        img.string(1, 2, "url", OLD_URL + b"\0")
        with self.assertRaisesRegex(ValueError, "found 2"):
            nvs_tool.current_ws_url(img.data)


class PatchTest(unittest.TestCase):
    def patch(self, new):
        img = device_image()
        before = bytes(img.data)
        entry, _ = nvs_tool.current_ws_url(img.data)
        nvs_tool.patch_ws_url(img.data, entry, new)
        return img, before, entry

    def test_shorter_url_is_nul_padded(self):
        new = b"ws://10.0.0.5:8765/"
        img, before, entry = self.patch(new)
        _, url = nvs_tool.current_ws_url(img.data)  # CRCs re-checked here
        self.assertEqual(url, new.decode())
        value = img.data[entry.off + ENTRY:entry.off + ENTRY + entry.size]
        self.assertEqual(bytes(value), new + b"\0" * (entry.size - len(new)))
        # Only the URL entry changed.
        span = img.data[entry.off + 2]
        end = entry.off + span * ENTRY
        self.assertEqual(img.data[:entry.off], before[:entry.off])
        self.assertEqual(img.data[end:], before[end:])

    def test_same_length_url(self):
        new = b"ws://192.168.1.99:8765/"
        self.assertEqual(len(new), len(OLD_URL))
        img, _, _ = self.patch(new)
        _, url = nvs_tool.current_ws_url(img.data)
        self.assertEqual(url, new.decode())

    def test_longer_url_does_not_fit(self):
        img = device_image()
        before = bytes(img.data)
        entry, _ = nvs_tool.current_ws_url(img.data)
        with self.assertRaisesRegex(ValueError, "fit in place"):
            nvs_tool.patch_ws_url(img.data, entry, OLD_URL + b"x")
        self.assertEqual(bytes(img.data), before)


class FindNvsTest(unittest.TestCase):
    def test_finds_the_nvs_partition(self):
        table = bytearray(b"\xff" * 0xC00)
        for i, (label, offset, size) in enumerate([(b"nvs", 0x9000, 0x4000), (b"otadata", 0xD000, 0x2000)]):
            e = bytearray(32)
            e[0:2] = b"\xaa\x50"
            struct.pack_into("<II", e, 4, offset, size)
            e[12:28] = label.ljust(16, b"\0")
            table[i * 32:i * 32 + 32] = e
        self.assertEqual(nvs_tool.find_nvs(table), (0x9000, 0x4000))

    def test_missing_nvs_partition(self):
        with self.assertRaisesRegex(ValueError, "nvs partition not found"):
            nvs_tool.find_nvs(bytearray(b"\xff" * 0xC00))


if __name__ == "__main__":
    unittest.main()
