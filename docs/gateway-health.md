# Quiet Discord gateway health checks

`contrib/discord_health.py` is a read-only host-side probe for the standard
`matrix-mautrix-discord` systemd/Docker deployment. It uses only the Python
standard library and requires permission to read the service journal, inspect
that container, and read its generated configuration. It does not restart the
bridge, change configuration, send messages, or use account tokens. Configuration content is processed locally; only the two
boolean flags and a checksum are returned.

Run the probe on the bridge host:

```sh
sudo python3 /usr/local/libexec/matrix-discord-health.py --expected-sessions 2
```

Install that file from the reviewed `contrib/discord_health.py` source. Keep
its owner root and its mode 0755. This host-side installation is independent of
the Docker image and must be reapplied when rebuilding the host.

## Decisions

* A fresh gateway heartbeat acknowledgement is required for each expected
  session. An old READY event alone is not evidence that a connection is alive.
* Allow five minutes for initial startup or a brief ongoing recovery. Alert
  for recovery stalled at least five minutes, a heartbeat stale at least five
  minutes, invalid credentials, or a stopped service.
* Six or more disconnects for one account within fifteen minutes constitute a
  reconnect storm, even when individual reconnects succeed. Different accounts
  are not combined to manufacture a storm.
* Intentional account disconnection is quiet until a new connect is observed.
* Missing account coverage, malformed or truncated logs, ambiguous configuration,
  changing deployments, failed commands, and stale invocation identifiers are
  explicit unknown states, never a healthy result. Outgoing Matrix presence or
  custom-status forwarding being enabled is also an actionable finding.

The JSON output includes `status`, `needs_attention`, `findings`, and a stable
`notification_key`. A notification consumer should alert only when attention is
needed and the key differs from its last notified key. Reset that key after a
quiet result; do not announce successful routine checks. Age counters are not
part of the key. A key is a deduplication aid, not a delivery acknowledgement.

Exit status 0 means quiet, 1 means actionable, and 2 means unknown. Quiet includes
startup grace and a short current recovery; inspect `status` before describing a
result as healthy. An hourly consumer can miss short incidents between checks;
this is a periodic safeguard, not real-time paging.

The probe queries the current service invocation and rejects evidence older
than the current container start. It reads a bounded recent window plus a
bounded lifecycle tail, caps subprocess output and run time, and checks that the
service, container, and configuration remain unchanged throughout collection.
Only hashed account identifiers, counters, timing, locally classified diagnostic
categories, and numeric close codes are emitted. Raw journal messages and peer
close reasons are never returned. This checks gateway health, not end-to-end
message delivery or mobile notification delivery.

## Gateway diagnostics

The pinned gateway dependency emits low-volume `gateway_diagnostic` records for
server-requested reconnects, invalid sessions, socket failures, and heartbeat
failures. `gateway_health heartbeat_ack=true` is emitted at informational level
when Discord actually acknowledges a heartbeat. Full gateway debug logging is
not required or enabled. Existing retry/backoff, presence settings, client
identity, and read-receipt behavior are unchanged.

## Tests

```sh
python3 -m unittest discover -s contrib -p 'test_discord_health.py' -v
go test -race -count=1 ./...
go test -race -count=1 -timeout 180s github.com/bwmarrin/discordgo/...
```

The dependency suite must run without live Discord test credentials. Its
credential-gated integration tests are skipped in ordinary offline CI; unit
fixtures use local WebSocket servers, not Discord accounts.

## Quiet notification consumer

Install `contrib/discord_health_watch.py` on the monitoring Mac in a persistent
location outside disposable worktrees. Run it with `--host USER@HOST` and
`--expected-sessions 2`. It invokes the installed host probe through SSH with
strict host-key verification and batch authentication, and makes no remote
changes. Configure the destination privately; no host credentials belong in
this repository.

The consumer keeps a lock-protected, atomically replaced 0600 state file under
`~/.local/state/discord-health/` by default. Notify only when its JSON output has
`notify: true`. Repeated unchanged conditions are quiet; a quiet check resets
the key so a later recurrence can notify. SSH failures and malformed probe
responses are explicitly unknown, not healthy. Notification state records an
attempt, not a delivery acknowledgement; the scheduling service remains
responsible for delivering the notification. This is an hourly safeguard, not
a five-minute paging guarantee, even though the probe uses five-minute health
thresholds.

Run all probe and consumer tests together:

```sh
python3 -m unittest discover -s contrib -p 'test_discord_health*.py' -v
```
