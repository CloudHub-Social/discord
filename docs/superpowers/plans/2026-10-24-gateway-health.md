# Discord gateway diagnostics and quiet health checks

Goal: finish the authorized dependency cleanup, explain future disconnects without raw gateway debug logging, and notify only for stalled recovery or a reconnect storm.

Constraints: keep both outgoing presence flags false; no mobile identity changes, read-receipt changes, message tests, credentials in logs, or SableClient writes. Work only in CloudHub-Social/discord and CloudHub-Social/discordgo. Preserve existing callback semantics and retry/backoff. Retain rollback image before deployment.

Approach: repair legacy handlers only when the custom callback is absent; correct the obsolete avatar example. Add safe, low-volume gateway cause metadata, rather than logging payloads. Use a bounded read-only journal/service health probe and an hourly quiet notification check; no automatic restarts or credential changes.

Tasks:
- [x] Reproduce full dependency failures, add callback/fallback/concurrency regressions, fix and run full offline race suite plus examples.
- [x] Inspect current filtered live lifecycle evidence, add safe disconnect diagnostics and tests; do not invent a retrospective root cause.
- [x] Add and test a health probe: missing readiness/outage over 5 minutes, at least 6 disconnects per account in 15 minutes, collection failure explicit, brief recovered disconnects quiet, fresh service invocation only, bounded time/output and no raw messages or secrets.
- [ ] Pin tested dependency; run bridge race suite and full pinned dependency suite in CI. Review diff, publish changes, deploy verified artifact with rollback guard and verify configuration, READY and lifecycle.
- [ ] Create an hourly condition-watch automation using the installed read-only health probe, suppress duplicates, and document actual verification and limits.

Review focus: callback duplicates, handler self-removal and concurrent once events; stale service invocation/log truncation; invalid credentials; SSH failure reported as unknown not healthy; notification duplicates.

Execution: inline; no subagent tool is available. Independent review is unavailable, so perform and explicitly identify a separate self-review pass.
