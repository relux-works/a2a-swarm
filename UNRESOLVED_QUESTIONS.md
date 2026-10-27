# Unresolved questions

## Issue #89: exact trigger in the reported live lanes

Private deterministic fixtures reproduce the post-type refusal when a Claude
registry status or statusUpdatedAt changes after typing, while the normalized
composer start stays unchanged. Stable named Claude 2.1.309/2.1.310 and Codex
fixtures submit successfully. The field report did not preserve the before/after
keys, so the precise live trigger, including the Codex lane's installed-image
behavior, remains unverified.

Any future field investigation belongs to a separately authorized host-keeper
window. This fix uses the issue's loud-surfacing alternative: pending typed
attempts become SUSPECTED-DEAF and report once to the existing #system room after
ten minutes. Recovery remains manual; existing nonempty-input fences are kept.
