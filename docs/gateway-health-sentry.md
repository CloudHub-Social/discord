# Matrix-Discord health monitoring through Sentry

Status (2026-10-24): the status-only adapter and systemd units have passed
local health tests and target-host unit/sandbox validation. Sentry management
configuration and production activation remain blocked by the current client's
authenticated management access, not by an outstanding owner sign-in reminder.
The existing ChatGPT health automation remains enabled and unchanged. Merely
installing these files does not activate the timer or establish alert delivery.

## Scope

Reuse the read-only `discord_health.py` probe and its existing five-minute
startup/recovery/heartbeat thresholds and per-account reconnect-storm threshold
(six disconnects in fifteen minutes). Do not change bridge behavior, presence,
read receipts, credentials, or the reconnect policy.

A systemd timer on the bridge host runs once a minute. Its adapter sends Sentry
`in_progress` and terminal `ok`/`error` check-ins sharing one execution ID.
Sentry receives only that ID, status, and the `production` environment, not
probe output, gateway payloads, account identifiers, or connection secrets.
The timer does not depend on a Mac or a ChatGPT task. Missing check-ins can
expose a stopped host, a stopped timer, or a broken Sentry delivery path; they
do not by themselves identify the cause.

A quiet probe result (including startup grace, short recovery, and intentional
account disconnection) produces `ok`. An actionable or unknown result produces
`error`; a failed health check must not be described as a proven bridge outage.
Unknown includes invalid output, inconsistent exit/attention state, timeout,
and inability to collect evidence. Delivery failure never becomes a success.
The adapter does not automatically restart anything.

## Sentry configuration required before activation

Use the existing intended Sentry organization and its appropriate project after
reading their current settings. Do not repurpose the PCP scheduler's monitor.
Check the organization's monitor quota before adding a monitor; no billing or
plan change is authorized by this change.

Create or reconcile only a dedicated monitor with:

- Slug: `matrix-discord-health`.
- Name: `Matrix-Discord health check`.
- Schedule: `* * * * *` (every minute), UTC.
- Environment: `production`.
- Check-in margin: 1 minute; maximum runtime: 1 minute.
- Failure threshold: 2 consecutive failed/missed check-ins.
- Recovery threshold: 1 successful check-in.

This adds up to about one extra minute before a persistent probe finding
creates a Sentry incident. It is not a promise of notification latency.
Configure an enabled notification rule for this monitor using the owner's
existing appropriate notification destination. A monitor without a verified
alert route is not a completed migration. Preserve unrelated alerts/monitors.
Sentry's API and current UI may differ; read back the stored settings rather
than assuming a submitted request succeeded.

Copy this monitor's generated HTTP check-in URL to a root-owned 0600 file at
`/etc/matrix-discord-health/sentry-cron-url`, with a root-owned 0700 parent
directory. Use the existing authorized credential transfer route, not a shell
argument or committed environment file. Only the Sentry ingestion credential
is needed on the bridge host; do not put a Sentry management token there.

The adapter accepts Sentry SaaS generated `cron` or `crons` URLs for this exact
monitor slug, with TLS and a Sentry ingestion hostname. It refuses redirects,
URL query/fragment additions, userinfo, and other destinations. Self-hosted
Sentry is outside this implementation's current scope.

## Installation and verification

Install the existing host probe and the new adapter from the reviewed source
as root-owned 0755 files:

- `contrib/discord_health.py` -> `/usr/local/libexec/matrix-discord-health.py`.
- `contrib/discord_health_sentry.py` -> `/usr/local/libexec/matrix-discord-health-sentry.py`.

Install both `contrib/systemd/matrix-discord-sentry-health.*` files in
`/etc/systemd/system/`. The service uses systemd `LoadCredential`; verify that
this directive is supported on the target host. It requires root access to
inspect the existing container, journal, and generated bridge configuration,
but does not restart or write to them.

After the credential, monitor, and alert route are configured, validate units
with `systemd-analyze verify`, reload systemd, and start the oneshot service.
Read back the matching check-in ID in Sentry and confirm the correct monitor,
project, environment, and terminal result. Then enable/start the timer and
verify a subsequent timer-triggered check-in in Sentry. An HTTP 2xx response or
an active local timer alone is not that verification.

Only after that verification, disable the existing **Discord Bridge Health**
ChatGPT automation (preserve its identity for rollback). Do not run both as
permanent duplicate alert consumers. An actual failure-notification delivery
remains unverified until observed; do not deliberately break the live bridge
to test it. Safe fixture tests below cover error mapping and transport failures.

Rollback: stop/disable only this timer and re-enable the original ChatGPT
health task. Preserve the live bridge configuration and image. Installation
files are not yet managed by the user's Ansible inventory; account for them
when rebuilding the host.

## Tests

```sh
python3 -m unittest discover -s contrib -p 'test_discord_health*.py' -v
```

The Sentry transport is tested at its network boundary without a live account.
These tests do not establish that a production Sentry monitor exists or that
notifications arrive.

References: Sentry's current HTTP Cron monitoring documentation and monitor API.
https://docs.sentry.io/product/monitors-and-alerts/monitors/crons/getting-started/http/
https://docs.sentry.io/api/crons/create-a-monitor/

## Verification record: 2026-10-24

- All 62 Python health tests pass, including two added regression tests for
  missing, unknown, malformed, or inconsistent probe status. Those regressions
  failed before the classifier fix and pass afterward.
- The full pinned Discord dependency race suite passes with live-test
  credentials explicitly empty.
- Target-host systemd-analyze verify passes for both units on systemd 255.
- A temporary oneshot using the same sandbox restrictions successfully ran the
  existing read-only probe against both expected accounts. This proves local
  collection works under the restrictions, not Sentry delivery.
- The local full bridge race build was blocked by missing olm/olm.h. Use the
  existing Linux CI (which installs libolm) for bridge tests and full lint;
  do not treat the dependency-only suite as the whole bridge suite.
- Separate author review checked endpoint restrictions, redirect refusal,
  failure classification, credential privacy, timer independence, and rollout
  gates. No independent reviewer result is claimed.
- Authenticated Sentry monitor/project/quota/alert-route checks and matching
  manual and scheduled check-in readbacks are still required. A browser sign-in
  does not itself give this Chat session browser-control or Sentry API access.

Do not activate the timer, claim notification delivery, or retire the existing
health consumer until the Sentry verification steps above actually succeed.

### Protocol-error review follow-up

Sentry's PR review identified an uncaught HTTP protocol-error path. Reproduced
BadStatusLine, LineTooLong and UnknownProtocol escaping the adapter, including
an aborted real-probe execution. Two new regression methods failed before the
minimal HTTPException catch and pass afterward. These failures now produce
only the fixed delivery-failure code while retaining the local probe result.
The full Python suite now contains 62 tests.
