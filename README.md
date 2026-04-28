# Codex Canvas Local

Local web UI for invoking Codex image generation.

## Local

Default local address:

```powershell
.\bin\codex-canvas-local.exe
```

```text
http://127.0.0.1:8765/
```

The port can be changed:

```powershell
.\bin\codex-canvas-local.exe --port 9000
```

or with a full listen address:

```powershell
.\bin\codex-canvas-local.exe --addr 127.0.0.1:9000
```

## Audit Log

Accepted generation requests are appended to:

```text
data/audit.jsonl
```

Each line is one JSON event with the job ID, timestamp, Cloudflare Access email
when present, client IP, user agent, original prompt, Codex args, Codex stdin,
session workdir, and reference image paths. The service rejects a new job if the
audit event cannot be written.

Quick inspection:

```powershell
Get-Content .\data\audit.jsonl | ConvertFrom-Json | Select-Object createdAt,email,ip,jobId,prompt
```

## Public Access

Current public hostname:

```text
https://pic.io99.xyz/
```

The hostname is routed through Cloudflare Tunnel `claw-tunnel`.

Important: this tunnel is managed remotely in Cloudflare Zero Trust. The local file
`%USERPROFILE%\.cloudflared\config.yml` only keeps the tunnel ID and credentials
file path. Public hostname ingress rules such as `desk.io99.xyz` and
`pic.io99.xyz` are configured in Cloudflare, not in the local config file.

## Cloudflare Access Login

`pic.io99.xyz` is protected by Cloudflare Access using email One-time PIN login.

Configuration path in Cloudflare dashboard:

```text
Zero Trust -> Access -> Applications -> Self-hosted application
```

Application settings:

```text
Name: Codex Canvas
Public hostname:
  Subdomain: pic
  Domain: io99.xyz
  Path: empty
Session duration: 24 hours
```

Authentication:

```text
Identity provider: One-time PIN
Accept all available identity providers: On
Apply instant authentication: optional
Authenticate with Cloudflare One Client: Off
```

Policy:

```text
Action: Allow
Include: the allowed email address or allowed email domain
```

Expected login flow:

1. Open `https://pic.io99.xyz/`.
2. Cloudflare Access asks for an email address.
3. Cloudflare sends a one-time PIN to that email.
4. Enter the PIN.
5. The browser is redirected to the Codex Canvas web UI.

## Tunnel Notes

Create or repair the DNS route:

```powershell
cloudflared tunnel route dns claw-tunnel pic.io99.xyz
```

If the tunnel needs to be restarted manually:

```powershell
cloudflared tunnel run
```

If the public hostname returns Cloudflare tunnel `404`, check the remote tunnel
configuration in Cloudflare Zero Trust first. A local ingress edit may not take
effect because the connector pulls remote-managed configuration from Cloudflare.
