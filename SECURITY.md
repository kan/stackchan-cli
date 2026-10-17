# Security Policy

## Supported Versions

stackchan-cli has no tagged releases yet. Only the latest commit on `main` is supported.

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately:

1. **Do NOT open a public issue.**
2. Use GitHub's private vulnerability reporting:
   [Report a vulnerability](https://github.com/kan/stackchan-cli/security/advisories/new)
   (the **Security** tab of this repository). Include:
   - Description of the vulnerability
   - Steps to reproduce
   - Potential impact
3. You will receive an acknowledgment within 7 days.
4. A fix will be published as soon as possible.

## Scope

stackchan-cli is a personal tool that controls a StackChan robot on the local network.
Keep this deployment model in mind when assessing an issue.

The following are in scope:

- Command injection or argument injection through the CLI, the REPL, script files
  (`source`), or the daemon's command forwarding
- Unintended exposure of the daemon IPC (`127.0.0.1:8770`) beyond the local host
- Path traversal or unintended file writes (captures, avatar payloads, logs, NVS backups)
- `scripts/nvs_set_ws_url.py` writing anything other than the intended NVS entry to the device
- Child processes (gateway, VOICEVOX engine) launched from an unexpected path

Known design limitations (not vulnerabilities on their own):

- The daemon IPC has no authentication; any process of any local user can send it commands.
- With `STACKCHAN_TOKEN` unset, the gateway's device port (`:8765`) accepts any client on the LAN.
  Set a token on both the gateway and the firmware if your network is not trusted.
- Audio captured by `listen` is transcribed locally, but the device sends it over the LAN unencrypted.

Out of scope:

- Vulnerabilities in the stackchan-mcp gateway or firmware
  ([kisaragi-mochi/stackchan-mcp](https://github.com/kisaragi-mochi/stackchan-mcp)),
  VOICEVOX, or other upstream dependencies (report to the upstream project instead)
- Denial of service
- Attacks requiring physical access to the machine or the robot
- Social engineering
