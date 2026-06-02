"""Browse for _stackchan-mcp._tcp.local. and print advertised addresses/port.
Run while a gateway is advertising:  uv run --with zeroconf python scripts/mdns_check.py
"""
import socket
import time

from zeroconf import Zeroconf, ServiceBrowser, ServiceListener


class Listener(ServiceListener):
    def _show(self, zc, type_, name):
        info = zc.get_service_info(type_, name, timeout=3000)
        if not info:
            print(f"  {name}: (no info)")
            return
        addrs = []
        try:
            addrs = [socket.inet_ntoa(a) for a in info.addresses]
        except Exception:
            addrs = [str(a) for a in info.parsed_addresses()]
        print(f"FOUND name={name} port={info.port} server={info.server} addrs={addrs}")

    def add_service(self, zc, type_, name):
        self._show(zc, type_, name)

    def update_service(self, zc, type_, name):
        self._show(zc, type_, name)

    def remove_service(self, zc, type_, name):
        print(f"REMOVED {name}")


zc = Zeroconf()
print("browsing _stackchan-mcp._tcp.local. for 8s ...")
ServiceBrowser(zc, "_stackchan-mcp._tcp.local.", Listener())
time.sleep(8)
zc.close()
print("done")
