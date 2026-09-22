# YouTube family strategy hint

Approved in conversation: new YouTube host tries a validated sibling TCP strategy; host keys, failure counters, pins and protocol/family isolation remain independent. Implement natively in current persistence wrapper, no release.

- [x] Add generated-profile regression tests: successful sibling seed; outgoing-only cannot teach; independent counters; exact saved/frozen state wins; family/protocol boundaries; expired and rotated donor; stale connection cannot teach.
- [x] Observe failing tests, then add a bounded in-memory hint (one per IP family, 300 seconds) to z2k-state-persist.lua. Seed once before circular only for a brand-new host record without a disk row; learn on a current-generation confirmed success transition. Never copy final/failure/SNI state.
- [x] Run profile tests and persistence/rotation regressions, Lua lint and full required shell suite if warranted by shared wrapper change. Review diff and document runtime-only hints. No tag/publish.

Validation completed: 169 relevant tests, Lua lint and diff check. Full shell suite not run; shared wrapper persistence, native rotation and TCP/QUIC detectors explicitly covered. Implementation review found no state migration or engine-fork change needed.
