# Tailgate

Expose services on your Tailscale network to a local network, without installing Tailscale on every device there.

Run Tailgate on one always-on computer on the remote LAN. It joins your tailnet and forwards ports from that computer to hosts on the tailnet, so a device that can't run Tailscale (a smart TV, a games console) can reach them.

```
TV  ->  computer-running-tailgate:8096  ->  (Tailscale)  ->  media-server:8096
```

Single binary for macOS, Windows and Linux. No separate Tailscale install needed.

## Download

Get the latest build from the **[Releases page](https://github.com/asellitt/tailgate/releases)**.

| Platform                       | File                   |
| ------------------------------ | ---------------------- |
| macOS (Apple Silicon)          | `tailgate-mac-arm64`   |
| macOS (Intel)                  | `tailgate-mac-intel`   |
| Windows                        | `tailgate.exe`         |
| Linux (x86-64)                 | `tailgate-linux-amd64` |
| Linux (ARM, e.g. Raspberry Pi) | `tailgate-linux-arm64` |

## Setup (tailnet owner, once)

1. In the Tailscale admin console, create an **auth key** (Settings → Keys) with a tag, e.g. `tag:tailgate`. Keys last at most 90 days, but that only matters for the first connection: tagged devices don't expire once joined.
2. Restrict what the node can reach in your ACLs, so a compromised machine exposes nothing else:

   ```json
   {
     "tagOwners": { "tag:tailgate": ["autogroup:admin"] },
     "acls": [{ "action": "accept", "src": ["tag:tailgate"], "dst": ["100.x.y.z:8096", "100.x.y.w:5055"] }]
   }
   ```

   Replace the IPs and ports with the hosts you want to expose.

3. Send the auth key to whoever runs Tailgate.

## Usage (the person running it)

1. Download the file for your system and run it. Your browser opens at `http://localhost:9473`.
2. Paste the auth key.
3. Add one row per service: **tailnet host**, **port**, and optionally a **local port** (defaults to the same as the port; only set it if two hosts share a port).
4. Click **Start**. The page then shows an address per row, like `192.168.1.50:8096`. Enter that address on the TV or device.

Settings are saved, so next time it reconnects on its own.

### Start at login

```sh
./tailgate --install
```

Uses a LaunchAgent on macOS, a registry Run key on Windows, and a systemd user service on Linux.

## Notes

- **The computer must stay awake.** Tailgate doesn't prevent sleep. If the machine sleeps, the TV loses the server.
- **Give the computer a fixed LAN address** (static IP or a DHCP reservation), or the address you typed into the TV will go stale.
- The control page only listens on `127.0.0.1`. Only the forwarded ports are open on the LAN.
- The page shows "direct" or "relay" per host. Relay means traffic is going through Tailscale's relay servers, which can be slower.

### macOS

Downloaded binaries are quarantined. Either right-click the file and choose Open, or run:

```sh
chmod +x tailgate-mac-arm64
xattr -d com.apple.quarantine tailgate-mac-arm64
```

macOS will ask to allow incoming connections the first time.

### Windows

SmartScreen will warn about an unsigned app: click More info, then Run anyway. Allow the firewall prompt on first run.

### Linux

```sh
chmod +x tailgate-linux-amd64
```

For a headless box (Pi, mini PC), run `--install`, then open `http://localhost:9473` from a browser on that machine, or tunnel the port over SSH: `ssh -L 9473:127.0.0.1:9473 user@host`.

## Where settings live

| OS      | Folder                                    |
| ------- | ----------------------------------------- |
| macOS   | `~/Library/Application Support/tailgate/` |
| Linux   | `~/.config/tailgate/`                     |
| Windows | `%AppData%\tailgate\`                     |

`config.json` holds the host list. `ts/` holds the Tailscale node state. Delete the folder to start fresh (you'll need a new auth key).

## Building

Needs Go 1.22+.

```sh
./scripts/build.sh      # cleans dist/ and builds every platform
./scripts/release.sh v0.1.0   # tags and pushes; GitHub Actions publishes the release
```

## License

MIT.
