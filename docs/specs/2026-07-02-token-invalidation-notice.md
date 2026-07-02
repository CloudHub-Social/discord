# PRD: Notify users when their Discord token is invalidated

**Source issue:** [CloudHub-Social/discord#30](https://github.com/CloudHub-Social/discord/issues/30) — "Bridge can sometimes have it's token invalidated"
**Status:** Draft
**Author:** Claude (spec drafted on behalf of Evie Gauthier)
**Date:** 2026-07-02

## Problem Statement

Discord periodically invalidates a bridged user's token — either via a forced password reset or an anti-abuse flag on the account — which disconnects the bridge's gateway session with a `4004`/`40002`-class error. The bridge already detects this correctly (`invalidAuthHandler`, `handlePossible40002`) and pushes a `StateBadCredentials` [BridgeState](https://mautrix.org/docs/bridging/asmux/) event, but it never posts a message the user will actually see in their Matrix client. `BridgeState` is a webhook-only push consumed by clients that specifically render bridge status (e.g. Beeper); self-hosted users chatting through a generic Matrix client see nothing. There is even a standing `// TODO send notice?` at `user.go:866` (this worktree's branch) marking this gap in an adjacent codepath.

The practical effect: bridging silently stops. The user notices only when they realize messages have stopped syncing, at which point they've usually lost hours or days of messages and have no in-context prompt telling them to run `login` again.

> **Note on code references in this spec:** this worktree's `main` branch is several months behind what's actually deployed. Production (`ghcr.io/cloudhub-social/discord:main`, running on `matrix-cloudhub-1.cloudhub.social`) is built from `cloudhub/main`, which already contains a large amount of presence/reconnect work not present in this checkout — including an on-demand guild-presence subscription mode and a proper exponential-backoff reconnect system (see [Findings from Production](#findings-from-production-matrix-cloudhub-1cloudhubsocial) below). Line numbers below are given against this worktree's `user.go` for readability; the equivalent functions on `cloudhub/main` are `invalidAuthHandler` (`cloudhub/main:user.go:2008`) and `handlePossible40002` (`cloudhub/main:user.go:2017`) — same gap, different surrounding code. **Any implementation work should branch from `cloudhub/main`, not from this worktree's `main`.**

## Findings from Production (matrix-cloudhub-1.cloudhub.social)

SSH'd into the production host and pulled `journalctl -u matrix-mautrix-discord.service` (systemd captures the container's stdout directly; docker's own log driver is disabled — `--log-driver=none`, see `/etc/systemd/system/matrix-mautrix-discord.service`). Journal retention is only ~3.5 days (4GB cap), covering 2026-06-29 09:11 through the present (2026-07-02 17:5x CEST).

**The one full token invalidation in the retained window** happened at **2026-07-02T01:51:37Z**, for `@evie:cloudhub.social` — almost certainly the event that prompted this issue (filed 2026-07-02T15:39:54Z). The log line is exactly the `invalidAuthHandler` message: `INF Got logged out from Discord due to invalid token`.

That invalidation was not an isolated blip. Working backward from it, the gateway session was stuck in a **rapid reconnect loop, cycling roughly every 6 seconds**, for at least the preceding several minutes (the pattern extends further back than the retained window can confirm — `Open()` calls for `@evie` at this cadence appear as far back as the journal goes, **2026-06-30T23:16**, meaning this has plausibly been going on for days, not minutes). Over the full retained window, `@evie`'s session alone accounts for **1,611 gateway `Open()` calls**, versus **3,445** occurrences of gateway close `4002` ("Error while decoding payload") and **2,550** of close `4003` ("Not authenticated") — i.e. thousands of failed reconnect cycles, not a handful. Each cycle follows the same shape:

```
Open() called → Op 10 Hello → full READY (fresh IDENTIFY, not Resume — the
merged_presences payload is a complete 24KB re-seed every time, and Discord's
protocol-level sequence numbers restart low each cycle) → ~1-2s of normal
activity, often including "Subscribing to guild on demand guild_id=..." (the
on-demand opcode-14 presence subscription added in PR #20) → gateway closes
with 4002 or 4003 → reconnect scheduled → repeat ~6s later
```

The backoff meant to slow this down doesn't engage: `scheduleReconnect()` computes delay from `reconnectAttempts`, but `onConnectionEstablished()` resets `reconnectAttempts = 0` on every READY/RESUMED — and every cycle *does* reach a brief READY before dying again a second or two later. So the failure counter never accumulates past 1, and the reconnect delay stays pinned at the 2-second base indefinitely (confirmed in the logs: `Scheduling Discord gateway reconnect with backoff attempt=1 delay=2000` repeats unchanged, cycle after cycle, rather than escalating). The bridge ends up performing a full re-IDENTIFY roughly every 6 seconds, sustained — which is itself a strong automation fingerprint independent of whatever is in each individual payload.

At **01:51:37Z**, after roughly 35 such cycles in the preceding ~3.5 minutes (and many more before that), Discord stopped merely closing the socket and killed the token outright — the next `Open()` got `InvalidAuth` immediately after `Hello`, before this worktree's request even reached a subsequent close-code line.

**As of this writing (checked live, 2026-07-02T16:03 UTC)**, the current session is stable — no reconnect loop, sequence numbers climbing normally, heartbeat/presence ticking fine — so this is not an active fire *right now*. But the storm-death-storm pattern (bursts of ~10 reconnects/minute for tens of minutes, occasional gaps, at least one confirmed token kill) recurs throughout the entire retained log, including a burst that was still running at **17:42 CEST (15:42 UTC) — three minutes after this issue was filed.**

**Assessment:** the reconnect storm looks like the proximate cause of (at least this instance of) the invalidation this issue is about, not a coincidence. **Update from a second debug pass:** a proper statistical test across the full retained log (READY events with vs. without a nearby on-demand `SubscribeGuild` call, and a per-guild breakdown across all three guilds this account has ever subscribed) confirmed this isn't just correlation in one tight sample — on-demand `SubscribeGuild` calls precede a gateway close 92-99% of the time, consistently, while READY events with no subscribe call nearby almost never do. This moves the on-demand guild-subscribe call from "leading hypothesis" to "confirmed trigger" — see [Debug Plan](#debug-plan) below for the full methodology, the remaining open question of *why* Discord's gateway rejects it, and a zero-code mitigation available today.

**Given the severity of these findings, both the backoff-reset bug and the SubscribeGuild correlation are pulled into this spec's scope as P0-B below**, rather than left as a deferred follow-up.

## Debug Plan

*(Written per `engineering:debug` methodology — reproduction, isolation, diagnosis, fix. This is a debug write-up produced from the production evidence above and a `cloudhub/main` code read, not a live interactive session; the "reproduction" for the confirmed bug is a state-machine unit-test scenario, since re-triggering it against real Discord isn't something to do casually against a live user's account.)*

### Bug 1 — backoff-reset defeats exponential backoff during flapping connections

**Reproduction**
- Expected: after N consecutive failed connection attempts, `scheduleReconnect()`'s delay should escalate (2s → 4s → 8s → … capped at `reconnectBackoffMax` = 10min).
- Actual: delay stays pinned at the 2s base indefinitely as long as each attempt achieves a brief READY before dying again — exactly what happened in production (`attempt=1 delay=2000` unchanged across at least 35 consecutive cycles before the token died).
- Repro without live Discord: drive the reconnect state machine directly — fire `onConnectionEstablished()` immediately followed by a simulated disconnect, in a loop, and assert `reconnectAttempts` (or the delay `scheduleReconnect()` computes) across iterations.

**Root Cause (confidence: high — read directly from `cloudhub/main:user.go`, consistent with every production log sample)**
`onConnectionEstablished()` (`cloudhub/main:user.go` ~line 1866) unconditionally sets `reconnectAttempts = 0` whenever READY or RESUMED fires, with no check that the connection was actually stable for any length of time. Since every storm cycle reaches READY (confirmed by the identical ~24KB `merged_presences` payload logged each time) before dying 1-2s later, the very next `scheduleReconnect()` call always computes its delay from a freshly-zeroed counter.

**Fix**
Don't zero the backoff counter on READY alone — only after the connection has been up for some minimum "probation" duration (e.g. a `stableConnectionThreshold` of 30s). Start a short timer on READY/RESUMED; zero `reconnectAttempts` only when that timer fires, and let it be superseded (leaving the counter untouched) if the connection drops before then. Structurally this mirrors the existing `readyWatchdog` pattern already in the file (which handles the *opposite* case — no READY within timeout) — it's a very similar shape of fix, not new machinery.

**Prevention**
- Unit test: a sequence of READY→disconnect cycles all faster than the stability threshold — assert the delay strictly escalates instead of staying pinned, up to the cap.
- Unit test: one connection that survives past the threshold, then disconnects — assert the counter resets to 0 (regression guard so the fix doesn't break the legitimate "healthy connection recovers from a later blip" case).

### Bug 2 — on-demand `SubscribeGuild` call is the direct trigger (root cause confirmed via statistical correlation, 2026-07-02 follow-up pass)

**Update:** a second debug pass ran a proper causal test against the full 3.5-day log (not just the tail-end storm sample the first pass used), via a small Python script executed on the production host over the already-open SSH session (log analysis only, no DB access). This moved Bug 2 from "three ranked hypotheses" to "confirmed trigger, mechanism still unexplained."

**Reproduction**
- Expected: `SubscribeGuild()` (opcode 14) for a guild with active Matrix-side presence interest succeeds silently, like any other authenticated gateway command.
- Actual, measured across all 1,610 READY events and 1,584 on-demand subscribe calls in the retained window:
  - READY events **followed by an on-demand `SubscribeGuild` call within 2s**: 1,343 events, **92.4%** died (gateway close 4002/4003) within that same 2s window.
  - READY events **NOT followed by a subscribe call within 2s**: 267 events, **0%** died within 2s. (Most of these still died within ~6-7s for other reasons — this account has been chronically unstable since at least 2026-06-30T23:16 — but a genuine subset, 33 of 267, stayed up 5+ minutes, up to ~3.3 hours in the best case, with no subscribe call fired nearby.)
  - Broken down by which guild was subscribed, the death rate is essentially identical across all three guilds this account ever subscribed to: `1261784144892006471` (1,253 attempts, 99.2% followed by close), `1124071202265239633` (235 attempts, 99.1%), `811981530192543784` (96 attempts, 97.9%). **This rules out a guild-specific payload issue** — it's not that one guild's channel set is malformed; the on-demand `SubscribeGuild` call is fatal essentially every time it fires, for any guild.

**Root Cause (confidence: high on *what* triggers it, unconfirmed on *why* Discord's gateway rejects it)**
The on-demand guild-presence subscribe call (`reconcileGuildSubs()` → `sendGuildSubscribe()` → `SubscribeGuild()`, opcode 14) is, empirically, close to a deterministic trigger for an immediate gateway close on this account/session — not a rare race, not specific to one guild's data. The *mechanism* — what Discord's gateway specifically objects to in this payload or its timing, on this particular account — is not established by log analysis alone; that requires either a raw gateway frame capture or reproducing against a disposable test account (neither done in this pass; see Isolation below). Ranked candidate explanations for *why*, now demoted from "leading hypotheses" to "candidate mechanisms" since the *what* is settled:
1. Something account/session-level makes Discord's gateway reject opcode 14 from this specific token near-unconditionally (e.g. the account may already be under some anti-abuse posture predating the confirmed hard invalidation — the chronic instability traces back to 2026-06-30T23:16, well before the 2026-07-02T01:51:37Z token kill, which would fit "already flagged, escalated later" better than "fresh trigger on July 2").
2. A structural issue in how/when the reconciler sends the payload relative to connection state (the session-lifecycle race originally proposed) — less likely now given the near-100% consistency across guilds and the whole multi-day window, since a race would typically produce noisier, less deterministic results.
3. The `s.RLock()`-vs-`s.Lock()` concurrency question in `beeper/discordgo`'s `SubscribeGuild()` (`wsapi.go:686`) — still worth a quick code check, but a low-probability explanation for a >97% consistent effect across days and guilds.

**Fix**

*Immediate, zero-code-change mitigation (recommend applying to production now, pending your go-ahead — this is a config change, not something to apply unilaterally):* set both `sync_discord_presence_to_matrix: false` and `sync_discord_status_to_matrix: false` in `config.yaml`. `discordReadEnabled()` (`cloudhub/main:user.go` ~line 1182) gates *all* opcode-14 sends on either flag being true — disabling both stops the on-demand subscribe call (and therefore, per the data above, should stop ~92% of the storm's rapid-death cycles) without touching code. This trades away Discord→Matrix presence/status sync until the underlying mechanism is understood.

*Proper fix (next phase, needs the isolation work below first):* depends on which candidate mechanism above turns out to be true. If it's account-level (candidate 1), the fix may be "don't send opcode 14 from this account at all until Discord's side clears," which is really the same as the immediate mitigation, made permanent/automatic. If it's a payload/timing defect (candidates 2-3), it's a targeted code fix once isolated.

**Prevention**
- Regardless of root cause, add a **circuit breaker**: track whether a connection dies within N seconds of the bridge's own most recent `SubscribeGuild` send; if that happens repeatedly in a short window, auto-disable on-demand subscription for that user's session (log loudly, don't just silently degrade) rather than continuing to retry the same fatal action every reconnect. This is a defensive guard against this *class* of bug recurring even after today's specific instance is fixed.
- Add payload-content logging (the exact bytes/fields sent, not just "called") so a future recurrence doesn't need a second archaeological pass like this one.

**Mitigation applied and verified (2026-07-02T18:30 CEST / 16:30 UTC):** with sign-off, set both `sync_discord_presence_to_matrix: false` and `sync_discord_status_to_matrix: false` in production `config.yaml` (backed up as `config.yaml.bak-2026-07-02-pre-storm-mitigation`) and restarted `matrix-mautrix-discord.service`. Confirmed via 2+ minutes of post-restart logs: the fresh session reached READY, sequence numbers climbed normally (28 → 74 → 78+), zero `SubscribeGuild` calls (as expected — `discordReadEnabled()` now returns false), zero reconnects, and real traffic flowed normally (read receipts, typing indicators, message decryption). This is strong additional confirmation of the root cause beyond the correlational log analysis — removing the trigger removed the symptom in real time. **This is a mitigation, not the fix** — Discord→Matrix presence/status sync is now off for this account until Bug 1 and the deeper "why" of Bug 2 are resolved and it's safe to re-enable.

**Isolation steps still needed (next phase, not completed here):**
- Reproduce against a disposable/test Discord account with raw gateway frame logging, to distinguish the three candidate mechanisms above.
- **After Bug 1 ships**, re-observe production: does the storm stop being a *storm* even if individual 4002/4003 closes still occur? Given the new data, expect this to matter less than originally thought — Bug 2 now looks like the dominant driver, with Bug 1 (backoff never engaging) as the multiplier that turns "occasional failed subscribe" into "hundreds of reconnects per day." Fixing both is still recommended, but if only one ships first, the immediate mitigation (disabling presence sync) plus Bug 1's fix likely gets most of the benefit even before Bug 2's deeper mechanism is understood.

## Testing Strategy

The repo currently has exactly one test file (`formatter_test.go` — stdlib `testing` + `testify/assert`, `package main`). There's no existing harness for testing `User` handler methods, `BridgeState` sends, Matrix notice delivery, or the reconnect state machine — all three items below need some testable surface introduced, not an existing pattern to just follow.

### 1. Notification feature (P0-A)

- **Unit:** extract the "should a notice fire" decision (handler × `ManagementRoom` presence × config toggle × `wasLoggedOut` state) into a small pure function separate from the actual `SendMessage` call, so it's testable without a live homeserver. Table-driven, matching `formatter_test.go`'s style — cover `ManagementRoom` set/unset × toggle on/off × already-logged-out/not, asserting both the fire/no-fire decision and the exact notice body per failure-reason code.
- **Integration:** a test double for the Matrix intent's `SendMessage` to assert the notice is dispatched to the right room exactly once from `invalidAuthHandler`/`handlePossible40002`, and never from `disconnectedHandler` — the regression guard for "must not notice on transient disconnects."
- **Manual/staging:** one real end-to-end check against a disposable test account before shipping — revoke the token from Discord's own security settings, confirm the notice lands in the management room, and confirm re-logging in doesn't produce a duplicate.

### 2. Backoff-reset bug (P0-B.1)

A timing/state-machine bug, not a pure function — the test drives the state machine directly rather than asserting one input/output pair.

- **Unit:** expose (or extract to an internal/test-only seam) the `reconnectAttempts` + stability-timer state so a test can:
  - Simulate N consecutive READY→immediate-disconnect cycles (all faster than the stability threshold) and assert the computed delay strictly escalates instead of staying pinned.
  - Simulate a connection that survives past the threshold, then disconnects, and assert the counter resets to 0 (regression guard against overcorrecting).
  - Assert the delay never exceeds `reconnectBackoffMax` regardless of cycle count.
- Drive the counter-update logic directly rather than sleeping real wall-clock time through `time.AfterFunc` — the arithmetic is already isolated from the actual timer scheduling, so this stays a fast unit test.
- No separate integration/manual test needed for this one in isolation — its live verification is folded into item 3's manual check below, since it's the same incident.

### 3. SubscribeGuild → 4002/4003 correlation (P0-B.2)

Can't be meaningfully unit-tested yet since the root cause isn't confirmed (see Debug Plan) — the test plan here is about *isolating* the cause first.

- **Isolation (pre-fix):** against a disposable test account, reproduce the on-demand-subscribe flow with raw-frame logging enabled. Manual/exploratory, not automated.
- **Regression test (post-fix):** once the actual cause is confirmed, add a targeted unit test against that specific mechanism — can't specify further until isolation completes.
- **Manual/staging:** combined with Bug 1's fix, force several rapid disconnects on a test account and confirm the bridge backs off (escalating delay) rather than sustaining a tight retry loop, even if an occasional individual 4002 still occurs.

### Coverage gaps worth flagging in the PR description

- This PR introduces the first tests of `BridgeState`/Matrix-intent/`User`-handler code in the repo. Keep new test doubles scoped to what these three items need — don't build a general-purpose mock of the whole `User` struct as a side effect of this work.
- Given production is mid-pattern (recurring storm bursts as recently as hours before this spec), Bug 1's fix is safe to ship ahead of Bug 2's root cause being fully nailed down — it strictly reduces harm (slower retries) regardless of what's triggering individual closes. Don't block Bug 1 on finishing Bug 2's isolation.

## Goals

1. A user whose Discord token is invalidated receives a Matrix message in their bridge management room within seconds of the bridge detecting the disconnect, without needing a BridgeState-aware client.
2. The message clearly states that re-authentication is required and how to do it (i.e. points at the `login` command).
3. Reduce the median time between token invalidation and the user re-authenticating (currently unbounded/unmeasured — first release should add logging sufficient to measure this going forward).
4. Do not introduce duplicate or spammy notices on reconnect/retry churn — one notice per distinct invalidation event.

## Non-Goals

- ~~Preventing token invalidation outright~~ — **superseded, now in scope.** Originally scoped as a non-goal ("notify after the fact, don't eliminate the trigger"); the production findings below were severe enough that the reconnect-storm fix is now P0-B. What's still out of scope is root-causing *every* possible way Discord could ever flag an account — P0-B fixes the specific, evidenced mechanism found in production, not a general guarantee against all future invalidations.
- **A generic notification/webhook framework** (email, push, Slack, etc.). Scope is limited to a Matrix `m.notice` in the existing management room, consistent with how the bridge already reports errors (`portal.go:1194`, `MessageErrorNotices`).
- **Changing BridgeState/webhook behavior.** The existing `StateBadCredentials` push stays as-is for clients that consume it; this adds a second, human-readable channel, it doesn't replace the first.
- **Auto-relogin.** The bridge will not attempt to silently re-authenticate with a cached credential; Discord invalidation means the old token is dead, so re-auth necessarily requires user action (QR/token login).
- **Notifying users who have no management room.** Users provisioned purely through a third-party client via the provisioning API (`provisioning.go:210`) without ever DMing the bridge bot have no `ManagementRoom` to post into; for v1 these users fall back to BridgeState-only (unchanged current behavior) rather than the bridge inventing a room.

## User Stories

- As a bridged Discord user, I want to be told in the room I talk to the bridge bot that my session was signed out, so that I notice immediately instead of discovering it days later when messages stop arriving.
- As a bridged Discord user, I want the notice to tell me what to do next (re-run `login`), so that I don't have to go dig through documentation.
- As a bridged Discord user who gets a transient disconnect (network blip, Discord outage), I want to **not** be paged with a "log back in" notice, so that I don't get trained to ignore bridge messages.
- As a self-hosted bridge admin, I want the notice behavior to be configurable, so that I can turn it off if I'm already relying on BridgeState pushes and don't want duplicate noise.
- As a bridge maintainer, I want structured logs/metrics on invalidation events, so that the "why is Discord flagging accounts" investigation has data to work from instead of anecdotes.

## Requirements

### Must-Have — P0-A: notify the user

1. **Send a management-room notice on confirmed bad-credentials events.**
   - Trigger points: `invalidAuthHandler` (`user.go:1168`, gateway close 4004/4010-4014) and `handlePossible40002` (`user.go:1177`, REST `40002` action-required-verified-account).
   - Given the user has a non-empty `ManagementRoom`, when either handler fires, then the bridge sends an `m.notice` to that room stating the session was signed out and that the user needs to log in again, including the `login` command syntax.
   - Given the user has no `ManagementRoom` set, when either handler fires, then only the existing `BridgeState` push happens (no behavior change, no crash/error from a missing room).
2. **One notice per invalidation, not per reconnect attempt.** Given `invalidAuthHandler` already sets `wasLoggedOut = true` and calls `Logout`, the notice must fire exactly once per invalidation — reuse that existing dedup state rather than introducing a new counter that can drift from it.
3. **Do not notice on transient disconnects.** `disconnectedHandler` (`user.go:1156`) must remain notice-free; only the bad-credentials paths gain a room message. Confirm this by testing a plain network drop/reconnect does not produce a notice.
4. **Config toggle.** Add `bridge.token_invalidation_notices` (bool, default `true`) to `config/bridge.go` and `example-config.yaml`, following the existing `message_error_notices` pattern (`config/bridge.go:45`). When `false`, suppress the new notice but keep BridgeState push unchanged.
5. **Message content is specific per failure reason**, reusing the existing `Error` codes already set on the BridgeState (`dc-websocket-disconnect-4004`, `dc-http-40002`) so the two paths are distinguishable in support conversations without requiring log access.
6. **Structured logging for invalidation events**, capturing at minimum: Discord user ID, close/error code, timestamp, and whether a `ManagementRoom` notice was sent. This is the data foundation for measuring the fix's impact and for future incident investigation — no dashboard/alerting required for v1, just queryable logs.

### Must-Have — P0-B: fix the reconnect storm that's actually killing tokens

Pulled in from what were previously deferred/future-considerations items, given the severity confirmed in [Findings from Production](#findings-from-production-matrix-cloudhub-1cloudhubsocial) and [Debug Plan](#debug-plan) above. These touch different code (reconnect/backoff internals in `cloudhub/main:user.go`, not the two notification call sites) and should land as their own PR(s) against `cloudhub/main`, not bundled into the P0-A notification change — but both are P0 for this spec's overall goal of "users aren't silently logged out."

1. **Fix the backoff-reset bug (Debug Plan Bug 1, high confidence).** `onConnectionEstablished()` must not zero `reconnectAttempts` immediately on every READY/RESUMED — only after the connection has survived some minimum stability threshold (proposed: 30s), mirroring the existing `readyWatchdog` pattern. Acceptance: a simulated flapping-connection scenario (READY → disconnect faster than the threshold, repeated) shows the computed reconnect delay strictly escalating instead of staying pinned at the 2s base, up to `reconnectBackoffMax`.
2. **Fix the on-demand `SubscribeGuild` trigger (Debug Plan Bug 2 — trigger confirmed, mechanism still open).** Statistical correlation across the full retained log (not just the tail-end storm) shows on-demand `SubscribeGuild` calls precede a gateway close 92-99% of the time, consistently across every guild this account has subscribed to — this is no longer "a hypothesis," it's the dominant driver. Immediate action: apply the zero-code mitigation in the Debug Plan (disable `sync_discord_presence_to_matrix` and `sync_discord_status_to_matrix`) to stop active harm, pending sign-off. Follow-up: isolate *why* Discord's gateway rejects it (candidate mechanisms in the Debug Plan) via a disposable test account, then ship either a targeted fix or a permanent circuit breaker.
3. **Re-verify against production after both land.** Confirm the storm-death-storm pattern (bursts of ~10 reconnects/minute recurring across days) stops recurring — not just that no *new* full token invalidation happens, since the storm itself (thousands of reconnect cycles) is harmful independent of whether it happens to end in a kill each time.

### Nice-to-Have (P1)

1. Include a count of days/messages since last successful sync in the notice, if cheaply derivable, so the user understands the blast radius before re-logging in.
2. Provisioning-API webhook consumers (clients with no `ManagementRoom`) get an equivalent event pushed through the provisioning websocket/webhook if one exists, closing the gap called out in the non-goal above. Needs confirmation such a channel exists before committing to it (see Open Questions).
3. Rate-limit repeated notices for accounts that get invalidated, re-logged-in, and invalidated again in a short window (e.g. don't send more than one such notice per hour) to avoid notice fatigue if the account is caught in an invalidation loop.

### Future Considerations (P2)

1. Push the same "please re-authenticate" message through additional channels (email on file, push notification) if the bridge ever grows a notification framework generically — explicitly deferred, see Non-Goals.
2. **Historically-proposed op-14 mitigations are already shipped, not future work — noting this explicitly since earlier drafts of this spec (and the original GitHub issue's framing) treated op-14 as an open risk.** `cloudhub/main` commit 7e82ec1 "On-demand guild presence subscription (#20)" (merged 2026-06-30) made the opcode-14 guild subscription on-demand and opt-in: only subscribes a guild with recent Matrix-side activity, TTL-evicted, capped at 10 concurrent, paced 2s apart. That closed out the *subscribe-everything-on-connect* self-bot fingerprint this memory used to worry about. It did not, however, fully solve token invalidation — the mechanism that's actually still live is the reconnect storm now specified as P0-B above, which is a different bug (a backoff/timing defect, not a subscription-breadth problem) that happens to also involve the on-demand subscribe call as a correlated (not yet confirmed causal) factor.
3. Beyond P0-B's confirmed mechanism, there could be other, still-unknown ways Discord flags an account (the original issue's "should investigate why Discord is flagging the account" is broader than what P0-B covers). No further action proposed here — revisit only if invalidations continue after P0-B ships and its production re-verification (P0-B item 3) comes back clean.

## Success Metrics

**Leading indicators (days–weeks post-launch):**
- % of bad-credentials events that produce a management-room notice (target: ~100% for users with a `ManagementRoom`, measured from the new structured logs).
- Zero reports of duplicate/spammy notices for a single invalidation event within the first month.

**Lagging indicators (weeks–months):**
- Median time-to-relogin after invalidation, compared against a pre-launch baseline. We have one real data point from the production investigation: the confirmed invalidation at 2026-07-02T01:51:37Z had produced zero user-visible signal as of this issue being filed nearly 14 hours later (15:39:54Z) — the user only found out because bridging visibly stopped. That's the baseline to beat, not a hypothetical.
- Reduction in "bridge silently stopped working" style issue reports.

## Open Questions

- **(Engineering)** Does the provisioning API have any push/webhook channel that a `ManagementRoom`-less client could receive an equivalent notice through, or is `BridgeState` the only surface for those clients today? Determines whether P1-2 is even buildable.
- **(Engineering)** Is there an existing baseline for "time from invalidation to relogin" we can pull retroactively (e.g. from `BridgeState` history or gateway logs), or does the metric only start once the new structured logging (P0-A.6) ships?
- **(Product/Evie)** Should `token_invalidation_notices` default to `true` for all deployments, or should self-hosters who already monitor BridgeState be opted out by default? Spec assumes default-on since most self-hosted users use plain Matrix clients without BridgeState rendering.
- ~~Should the reconnect-storm/backoff bug be pulled into this same release?~~ **Resolved: yes** — now P0-B. Still recommend it lands as its own PR(s) against `cloudhub/main` rather than the same commit as the notification work, since the code paths don't overlap; "same spec, separate PRs" rather than "separate spec."
- **(Engineering)** Debug Plan Bug 2's *trigger* is now statistically confirmed (see the updated Debug Plan), but the *mechanism* (why Discord's gateway rejects on-demand `SubscribeGuild` near-deterministically, across every guild, on this account) still needs a disposable-test-account reproduction with raw frame capture — not achievable from log analysis alone.
- ~~Should the immediate zero-code mitigation be applied now?~~ **Resolved: yes, applied and verified 2026-07-02T18:30 CEST.** See the Debug Plan's "Mitigation applied and verified" note. Presence/status sync is off in production pending the real fix — remember to re-enable once P0-B ships and is confirmed stable.
- **(Engineering)** Is `matrix-cloudhub-1.cloudhub.social`'s only-3.5-days journal retention (4GB cap) enough for future incident investigation, or should retention/log volume be revisited? A large fraction of the retained volume is `DBG`-level presence/gateway noise (`PASSIVE_UPDATE_V2` diagnostic dumps, per-channel permission computation) that made this investigation slower than it should have been — worth a look independent of this spec, but out of scope here.

## Timeline Considerations

- No hard external deadline, but **P0-B is the time-sensitive item in this doc** — it's a live, recurring pattern hammering Discord's gateway at automation-like rates and has already produced at least one confirmed token kill. Start it in parallel with P0-A, not after.
- Suggested sequencing given the two P0 tracks don't share code:
  1. P0-B.1 (backoff-reset fix) first — it's fully diagnosed, low-risk, and strictly reduces harm regardless of Bug 2's outcome.
  2. P0-A (notification) and P0-B.2 (SubscribeGuild isolation) in parallel — independent workstreams, different people could own each.
  3. P0-B.3 (production re-verification) after both B items ship.
- P1-2 (provisioning webhook) depends on answering its Open Question before scoping; not time-sensitive.
