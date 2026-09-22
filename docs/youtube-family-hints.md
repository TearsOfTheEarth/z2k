# YouTube TCP family hints

New `yt_tcp` host records for `youtube.com` and its subdomains may start with a strategy recently confirmed by the existing success detector on another member of the family. Full host keys and independent failure counters are retained. IPv4 and IPv6 have separate hints; QUIC and other domains/pools are unchanged.

A hint is learned only from a new successful incoming observation belonging to the current strategy generation. Outgoing attempts, saved candidate rows, neutral outcomes, active server rejection and a manual freeze alone do not count as success. A hint lasts at most 300 seconds and is discarded if its source changes strategy/generation. At most two hints live in memory; they are intentionally not serialized because state.tsv does not distinguish confirmed successes from candidates.

Existing host state (including strategy 1 and manual frozen selections) wins. Only a previously unselected host may inherit a number; no pin, learned SNI, counters or connection state are copied. Once selected, normal per-host rotation and persistence apply. A reset of an existing record stays a reset and does not continuously reapply the hint.

After restart, per-host choices still restore normally. Family hints start empty until fresh success. No migration or deletion of existing state is required. This reduces cold starts for new siblings; it does not assert that all YouTube endpoints work with one strategy or fix stale strategies universally.

Validation: tests/test_profile_observation.sh (23), test_z2k_state_persist.sh (87), test_detector_persistence.sh (3), test_circular_core.sh (16), test_alert_detector.sh (23), test_quic_silence_detector.sh (17); all 169 passed. Lua lint and git diff --check passed. No live deployment or release performed.
