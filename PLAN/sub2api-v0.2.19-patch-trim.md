# Sub2API v0.2.19 minimal gateway patch candidate

## Contract

- Baseline: official v0.2.19, `dece26153621959ccd7635814f4b2d08304e681a`.
- Retain production rerank changes `e5874647e6`, `e7a317f013`, `4eccc37971`, `14ceb1b0e4`, `5902f68eac`, `297669f211`; explicit affinity `2bc1fc0664`; error-rule field preservation `ab6b00ed67`.
- Omit Responses WS keepalive `60c4d5c16b` and `63e761b816`; the gateway terminates Responses WS before Sub2API.
- Preserve upstream migrations 246/247/248 and complete runtime unchanged. Their execution requires the separately approved parent deployment and isolated data validation.
- No CPA/keeper/bridge, account/group/pool, retry-policy or provider credential changes. No commits, pushes, publication or production actions in this candidate phase.

## Work

- [x] Apply only the eight retained production changes, retaining newer upstream behavior.
- [x] Verify rerank/provider adaptation/usage, explicit-session/cache identity, and error field preservation using existing backend/frontend tests and a restored negative control.
- [x] Verify frontend typecheck/build, complete runtime inputs and owned-fork publication interface.
- [x] Leave a clean merge/sequencer state and report exact changed files, checks and residual risks for independent review.

## Candidate evidence

The source writer timed out after completing the candidate. Parent recovered the isolated worktree, verified 179 service and 143 handler/routes passing unit-test events (including subtests), 129 frontend tests, typecheck/build and the restored negative control. Runtime-input checks and actionlint pass. Independent review, commit/publication, actual image validation and production acceptance remain separate gates; no published release is claimed.

Parent release/deployment change: AI-gateway `trim-sub2api-patches-and-upgrade`.

## Frontend security repair (approved 2026-11-02)

- [x] Upgrade `vue-i18n`/`@intlify/message-compiler` to 11.4.14, `decode-uri-component` to 0.5.1, `follow-redirects` to 1.16.2 via minimal overrides; no new libraries or audit exceptions.
- [x] Resolve GHSA-83rm-6c8r-g728, GHSA-qr28-fpjm-wq7f, GHSA-mrwp-f3q8-3g9p; remaining high advisories are only the pre-existing `xlsx` exceptions. Audit checker passes on the unchanged policy.
- [x] Add real-usage i18n regression test (locale load/switch/merge, interpolation, fallback) and re-run focused i18n/modal suites, frontend-critical, typecheck and production build.
