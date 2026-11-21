# mailbridge

A small Go service that serves this Mac's **Apple Mail** and **Messages** databases over a
**read-only, authenticated HTTPS API** on the LAN. The data stays on the Mac; consumers (for
example `hitorro-fleet-mail-analytics`'s `BridgeMailSource`) pull it.

It is one static binary (pure-Go SQLite, `CGO_ENABLED=0`), so macOS Full Disk Access can be
granted to exactly that binary and nothing else.

## How it reads

- **Mail**: the newest `~/Library/Mail/V*/MailData/Envelope Index` for metadata, and the
  `.emlx` / `.partial.emlx` files for bodies. One directory walk builds an id→file index,
  rebuilt at most every 5 minutes on a miss.
- **Messages**: `~/Library/Messages/chat.db`, including text that only exists in
  `attributedBody` (decoder adapted from `message_mcp`).
- The live databases are **never opened by SQLite**. The bridge copies the database file and
  its `-wal` into a private 0700 snapshot directory, retrying if either changes during the
  copy, and queries the copy (`query_only`). The copy is re-checked at most once a minute
  (`--snapshot-age`) and refreshed only when the source changed. Mail and Messages are never
  locked or modified.
- The Envelope Index schema is undocumented and changes between macOS releases. Optional
  columns are discovered with `pragma_table_info` and come back empty when missing.

## API

All endpoints are `GET`. Everything except `/healthz` needs `Authorization: Bearer <token>`.

| Endpoint | Returns |
|---|---|
| `/healthz` | `{"ok":true,"version":…}`, no data |
| `/v1/status` | counts, max ids, newest dates and snapshot times for both sources |
| `/v1/mail/messages?since=<id>&limit=<1..1000>&received_after=<unix>` | metadata, oldest id first, plus `next_since` and `has_more` |
| `/v1/mail/messages/{id}` | `meta`, `headers`, `text`, `html`, `attachments` (name, type, size; no content), `partial`, `body_status` (`ok`, `not_downloaded` or `unparseable`) |
| `/v1/mail/messages/{id}/raw` | the RFC 822 bytes (`message/rfc822`) |
| `/v1/messages/chats?limit=` | conversations with participants, message count and last activity |
| `/v1/messages?since=<id>&chat=<id>&after=<unix>&limit=<1..2000>` | iMessage/SMS rows with decoded `text`, oldest first, plus `next_since` |

**Watermarks.** Ids are the databases' ROWIDs, which only grow. Store `next_since` and pass it
back as `since`. For a bounded first batch, add `received_after` (mail) or `after` (messages),
e.g. "the last 7 days".

There are no write endpoints, no SQL, and no filesystem paths in requests (ids are integers).
Attachment names are basenames only.

## Security

- **TLS**: a self-signed ECDSA P-256 certificate is generated on first run. Clients pin its
  SHA-256 fingerprint (`mailbridge fingerprint`, also logged at start-up).
- **Token**: 32 random bytes, generated on first run and stored in
  `~/Library/Application Support/mailbridge/token` (mode 0600; the bridge refuses a looser
  mode). It is compared in constant time and never printed.
- **Bind**: `--bind auto` listens only on the interface address that lies inside the
  allowlist (never `0.0.0.0`). If DHCP moves the address, the process exits and launchd
  restarts it on the new one.
- **Allowlist**: by default only `192.168.1.0/24` and loopback may connect.
  `--allow-tailscale` adds `100.64.0.0/10`, and `--allow` replaces the LAN list.
- **Rate limits**: 20 requests/s per source address (burst 60). Ten bad tokens in a minute
  lock that address out for a minute.
- **Logging**: an access log line has the source, method, path, query parameter *names*,
  status, bytes and duration. Query values, headers, tokens and message content are never
  logged.

## Install (on the Mac, no sudo)

```sh
scripts/build.sh                                  # on a Mac with Go: dist/mailbridge
scp dist/mailbridge scripts/install.sh you@mac:/tmp/mailbridge-stage/
ssh you@mac bash /tmp/mailbridge-stage/install.sh /tmp/mailbridge-stage/mailbridge
```

`install.sh` puts the binary at `~/Applications/MailBridge/mailbridge`, creates the token and
certificate, and prints the fingerprint. It writes the launchd user agent
`~/Library/LaunchAgents/com.geekychris.mailbridge.plist` (RunAtLoad, KeepAlive, log in
`~/Library/Logs/mailbridge.log`) and starts it. Extra arguments after the binary path are
passed to `serve`, e.g. `--allow-tailscale`.

Then grant **Full Disk Access**:

1. Open System Settings → Privacy & Security → Full Disk Access, and click **+**.
2. Press Cmd-Shift-G, paste `~/Applications/MailBridge/mailbridge`, click Open, and switch it on.
3. Restart the agent: `launchctl kickstart -k gui/$(id -u)/com.geekychris.mailbridge`

Until then `/v1/status` reports `no_access`. If the macOS firewall asks whether `mailbridge`
may accept incoming connections, click Allow.

**Upgrades reset the grant.** The binary is ad-hoc signed, and macOS privacy controls identify
ad-hoc code by its hash, so every new build needs Full Disk Access again: remove the old entry
with **−** and add the file again. `install.sh` only replaces the binary when its bytes differ,
and says so.

To remove it, run `scripts/uninstall.sh`. Add `--purge` to also delete the token and
certificate.

### Checking it without printing the token

```sh
curl -sk -H "Authorization: Bearer $(cat ~/Library/Application\ Support/mailbridge/token)" \
  https://<lan-ip>:18443/v1/status
```

## Development

```sh
CGO_ENABLED=0 go test ./...
```

The tests build **synthetic** Envelope Index and chat.db databases and .emlx files
(`internal/fixtures`). Rows are left in the WAL, as they are while Mail is running. The tests
cover snapshots, schema mapping, watermarks, MIME/charset parsing, attributedBody decoding,
auth, the allowlist, rate limits, the access log and TLS pinning. No real mail is used.
