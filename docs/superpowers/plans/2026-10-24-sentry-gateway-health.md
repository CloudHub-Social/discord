# Sentry gateway health implementation plan

Goal: replace the hourly ChatGPT health consumer with status-only Sentry Cron
monitoring driven by the existing read-only probe on the bridge host.

Constraints: CloudHub-Social/discord only; no SableClient writes; no presence,
read-receipt, client-identity, retry, or bridge-runtime changes. Preserve the
working ChatGPT automation until actual Sentry check-ins and alert settings are
verified. Do not create a new billing subscription or change unrelated monitors.

Approach: a dependency-free Python adapter and a one-minute systemd timer. Keep
health policy in the existing probe; let Sentry handle failed/missed check-ins
and incident recovery. This avoids model inference and the monitoring Mac.

- [x] Inspect current probe, repository, and available authenticated routes.
- [x] Write red tests for status mapping, delivery, credential privacy, malformed
  results, timeouts, TLS endpoint restrictions, and redirect refusal.
- [x] Implement the minimal adapter and run its tests green.
- [ ] Run the full existing health and bridge/dependency suites, lint, unit
  validation, and a separate author review before publishing the branch.
- [ ] With authenticated Sentry access, read/create the dedicated monitor and
  alert rule, store only the ingestion URL as a protected systemd credential.
- [ ] Install and verify both manual and timer-driven check-ins in Sentry, then
  disable only the former ChatGPT health automation.

Access findings: exposed MCP portals and local MCPJungle registration have no
Sentry tool; no Sentry CLI/config or Codex Sentry server is configured. The two
existing Vivaldi tabs point to the cloudhubsocial organization login page.
Bitwarden CLI is locked; Secrets Manager did not authenticate. The browser
connector has no originating browser context in this chat. No passwords,
management tokens, or ingestion credentials were printed or changed.

Review focus: never report unknown as healthy; never follow credential-bearing
redirects; do not depend on the bridge service for the monitor's own startup;
do not activate a timer without an authenticated and verified destination;
retain rollback/old consumer during the handover.

## Continuation ledger: 2026-10-24

Owner confirmed Sentry sign-in and explicitly requested activation and
verification. Reused the existing isolated feature worktree and branch.

- Completed: 60/60 health tests; pinned dependency race suite; target-host unit
  validation; read-only probe under the intended service sandbox.
- Fixed: classifier ignored status and could map unknown/missing/inconsistent
  evidence to success. Two new regression methods failed (eight cases), then
  passed after the minimal status/exit/attention consistency fix.
- Review: separate author self-review; no reviewer subagent was available.
- Ruling: use the existing Linux CI for full bridge race tests and lint because
  this Mac lacks libolm headers. Publish a review branch to run that CI, but do
  not merge a red or unverified build. This leaves local full-build evidence
  unavailable rather than changing the Mac's compiler dependencies.
- Ruling: inactive server-side installation may proceed after source validation;
  activation remains gated on authenticated monitor/alert/readback verification.
  This advances installation without introducing an unverified alert consumer.
- Remaining: Sentry management access, correct monitor/project/quota/destination,
  protected ingestion credential, manual and timer-driven provider check-in
  readback, and actual notification delivery observation.
- Preserve the existing Discord Bridge Health automation and all unrelated
  reminders, monitoring, subscriptions, bridge runtime and configuration.
