# AGENTS.md - Implemented Agent Notes

These records describe shipped decisions. Follow the [root instructions](../../../AGENTS.md), [documentation standard](../../../docs/AGENTS.md), and [Agent Note format](../README.md#the-file-format). The repository's verifier owns lifecycle-specific structure.

## Keep an implemented Agent Note current with what actually shipped

Keep paths, symbols, defaults, mechanisms, and other factual details current in the same change that alters them. Rewrite stale facts in place; do not append change history.

### This is not a license to rewrite the decision

Update factual realization in place. A reversal of the decision or its rationale requires a new Agent Note and a cross-link. A fully superseded old note may be deleted only through the consolidation rule in the [Agent Note rules](../README.md).

When a shipped note is unlikely to guide future work, move its complete triplet to `archived/` through the repository's archive procedure.
