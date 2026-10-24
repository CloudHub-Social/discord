# Discord Reliability Implementation Plan

> **For agentic workers:** Use the existing implementation agents for the independent bridge changes; root implements and integrates the dependency fix. Each task includes regression tests and review.

**Goal:** Make Discord connection health truthful, recover from transient startup failures, bound gateway handshakes, and separate status text from outgoing presence.

**Architecture:** Reuse the bridge's current session readiness, teardown guards, and capped reconnect scheduler. Add a bounded post-upgrade handshake in the maintained Gorilla-based discordgo fork. Keep Matrix-to-Discord presence and status disabled in production.

**Tech Stack:** Go, Gorilla WebSocket, mautrix, existing Go tests/race detector and GitHub Actions.

**Spec:** This document, Requirements and Non-goals below.

## Requirements and Non-goals

- Connected means the current session reached READY or RESUMED and has not disconnected.
- Stale session events must not make a replacement session appear healthy or schedule duplicate recovery.
- Transient startup failures enter existing capped backoff. Invalid credentials and intentional logout/disconnect stop recovery.
- A post-upgrade gateway handshake must finish within 60 seconds by default; per-session positive timeout overrides support tests and unusual deployments.
- HELLO/READY reads, partial compressed-message decoding, and handshake writes are bounded; deadlines and cancellation timers are cleared before normal listening.
- Status-text-only changes on user sessions use the existing settings API without asserting online presence.
- Both outgoing flags disabled means no outgoing presence or custom-status change.
- Activity handling must match the actual serialized payload; do not copy another client's aggregate activities into this session.
- Preserve the deployed bot opcode-1 heartbeat correction and user heartbeat format.
- No read-receipt behavior changes, client identity spoofing, unrelated upgrades, or SableClient mutations.
- Preserve original working directories and current production settings; changes are isolated, reviewed, tested, then deployed with rollback available.

## Review Focus

- Late READY/disconnect from a replaced session cannot mutate current connection health.
- Manual disconnect/logout during backoff cannot revive a session.
- Authentication failures stop retries while transient failures continue.
- Successful handshakes remain usable beyond the initial deadline.
- Text-only presence changes and explicit clears do not alter availability or replay aggregate activities.

## Task 1: Current-session health and startup recovery

**Files:** user.go; commands.go/provisioning.go only where the current health contract requires it; existing reconnect tests and focused regression tests.
**Interfaces:** Preserve User.Connected() bool and User.Connect() error; adapt existing scheduler and session-event handling.
- [x] Write regressions for a session object without READY, disconnect, replacement-session stale events, and startup failures.
- [x] Run tests against old behavior and capture expected failures.
- [x] Track readiness for the current session and route transient startup failure to scheduleReconnect.
- [x] Verify teardown/authentication guards, backoff/stability behavior, and race detection.
- [x] Commit isolated implementation for integration review.

## Task 2: Bounded gateway handshake

**Files:** discordgo/structs.go, discordgo/discord.go, discordgo/wsapi.go, discordgo/wsapi_handshake_test.go; bridge go.mod/go.sum pin during integration.
**Interface:** Session.GatewayHandshakeTimeout time.Duration; non-positive uses the 60-second default.
- [x] Test peers that never send HELLO and that send HELLO but no READY, expecting a timeout error and closed failed socket.
- [x] Demonstrate failures with the original Open implementation.
- [x] Set read/write deadlines after upgrade; clear both after handshake and before live goroutines.
- [x] Test successful post-timeout retry and normal traffic after the handshake deadline.
- [x] Run focused dependency regressions with the race detector, record baseline suite failures, review before publishing, and pin the exact immutable revision.

## Task 3: Presence correctness

**Files:** main.go, presence sections of user.go, example-config.yaml, focused presence tests.
**Interfaces:** Preserve configuration flag meanings; use existing custom-status settings API and presence sender.
- [x] Test text-only changes/clear, idle AFK, disabled flags, and activity payload semantics.
- [x] Demonstrate old behavior fails the relevant assertions.
- [x] Decouple text from availability and make activity handling explicit without replaying other clients' activities.
- [x] Run focused presence tests and race checks; commit isolated implementation.

## Task 4: Integration and deployment

- [x] Integrate reviewed commits into a clean branch from CloudHub main.
- [x] Verify exact dependency pin, combined diff, and all applicable tests.
- [ ] Obtain independent code review, create PR with problem/behavior/test evidence, and require CI success.
- [ ] Merge with a merge commit; verify merged-head image build.
- [ ] Retain rollback image and deploy only matrix-mautrix-discord.
- [ ] Verify exact running revision, both sessions READY, outgoing flags still disabled, and no repeated gateway errors.

## Verification Notes

The new handshake regressions fail on the original Open behavior, and the partial-compression cases also fail with socket deadlines alone. All targeted handshake tests pass with the race detector after the combined fix. The inherited discordgo full suite has two independently confirmed baseline failures: TestAddHandler panics because the fork uses a different event-handler contract, and examples/avatar calls an obsolete UserUpdate signature. The bridge CI will run the new dependency regressions explicitly against its pinned revision, alongside the full bridge suite.

## Verified implementation results

- Dependency candidate 0505d3fa1f9fd40e014bd51d8c97251dcdeb359e passed independent review and is published on CloudHub-Social/discordgo cloudhub-gorilla. The bridge pins v0.0.0-20261024002034-0505d3fa1f9f.
- Focused handshake race tests passed at the immutable module pin (9.454s). They include partial compressed header/JSON stalls and continued traffic after the deadline.
- Presence regressions failed on the baseline and passed on the reviewed candidate (10.470s).
- Connection/startup regressions failed on the baseline; the final health, recovery, stale-event, and failed-Open race selection passed (5.547s).
- The dependency fork retains two verified baseline test failures: legacy TestAddHandler and the avatar example UserUpdate signature. New handshake regressions are an explicit bridge CI gate alongside the full bridge race suite.
- Scope remains connection health/recovery ownership. Existing concurrent Login assignment and already-running slow room setup are not redesigned; queued stale work and final Connected publication are guarded.

- Integrated native gate: full bridge race suite, exact-pinned gateway race regressions, and repository pre-commit checks all passed. Linux CI remains the release gate.
