# Gateway health implementation record

Scope: dependency compatibility repairs, safe gateway diagnostics, bounded read-only health assessment, and quiet hourly notification consumption. Outgoing presence/status remain disabled. No mobile identity, read receipts, credentials, unrelated repositories, or service networking changes.

## Verified implementation
- Full dependency race suite and example compilation pass without live credentials; go vet passes.
- Full bridge race suite and full pinned dependency suite pass on the Mac.
- All 41 health assessment/collector/consumer tests pass on the Mac.
- The live pre-deployment probe successfully reads the running revision and both sessions; it correctly reports heartbeat evidence missing on the old image rather than calling old READY events healthy.

## Review
Separate author self-review; no independent reviewer tool was available.
- Preserved custom callback exclusivity and legacy handler self-removal/concurrent-once safety.
- Fixed review finding: diagnostics must retain error severity so error-only logging does not hide heartbeat failures. Regression observed RED then GREEN.
- Fixed review finding: reject heartbeat evidence older than current container start. Regression observed RED then GREEN.
- Fixed review finding: ambiguous duplicate presence keys, including invalid duplicates, must fail closed. Regression observed RED then GREEN.
- Watcher subprocess failures are unknown; raw stderr and peer error text are not returned; notification keys exclude changing counters and are protected by an atomic locked state store.

## Rulings
- Use matching Go 1.25/1.26 CI matrices already used by the bridge; inherited dependency CI targeted versions older than its module floor. Retire its incompatible golangci-lint 1.45 integration while preserving full tests, vet and changed-file format checks. Cost: that old linter is not an additional gate.
- A read-only host probe plus hourly consumer is the smallest compatible monitor. It is not real-time paging and can miss an incident wholly between checks.
- The probe install persists across image deployments but is not Ansible-managed; a host rebuild requires reinstalling from this repository.
- Existing brief disconnects have not yet been attributed to a verified root cause. Diagnostics improve future evidence; they do not justify claiming the historical cause is fixed.

Deployment and scheduled-consumer verification are recorded separately after actual execution; tests alone do not establish that the live service or scheduled notifications work.
