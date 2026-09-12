# Agent Notes


An **Agent Note** records a decision or proposal that affects the repository: the why, the alternatives, and the consequences that source code and ordinary reference docs cannot carry. This file defines where Agent Notes live, when to write one, and [the in-file format](#the-file-format).

## Layout and naming

Every Agent Note has two axes, both encoded in its path: `{lifecycle}/{class}/yyyy-mm-dd-topic-title.md`.

- **Lifecycle** is the top-level folder and the note's status. `proposed/` holds work that is under consideration, `implemented/` holds a shipped decision, and `rejected/` holds a considered proposal that was declined.
- **Class** is the nested folder and the kind of decision. The repository keeps this set closed so placement stays searchable and mechanically checkable.

The date in the filename is when the topic was first proposed. Cross-references use relative Markdown links, never bare numbers or informal references, so they remain checkable when a note moves between lifecycles.

The active lifecycle tree is the working inventory. Browse its class folders or search the repository; do not create a central index unless the repository has a specific, recorded reason to need one. Implemented records with little future decision value move to the separate frozen `archived/` tree described below.

## Classification

Each note belongs to one class from the repository's closed set. A typical set is:

| Class | What it covers |
|---|---|
| `feature` | A new user- or system-facing capability. |
| `bug-fix` | A correction prompted by a defect or a discovered gap. |
| `simplification` | Removal of code, behavior, or surface area without adding a capability. |
| `architecture` | A structural decision about the shipped source and its boundaries. |
| `process` | Tooling, policy, or workflow around the source and releases. |
| `testing` | Test infrastructure, coverage strategy, or verification design. |

Keep the architecture/process boundary explicit: architecture describes what the system ships; process describes the surrounding tooling and workflow. Do not add a `refactor` class when its subject is already covered by simplification or architecture.

## Archiving and deletion

Archive an implemented note when its decision is complete and its rationale is unlikely to guide future work. Keep it active when its alternatives, ownership boundary, negative guarantee, durable or wire semantics, security rule, or reintroduction condition remains useful. Never archive a proposed note; reject an obsolete proposal.

Keep a rejected note only while its rationale prevents a plausible mistake. Otherwise delete its English, Chinese, and sidecar files together. Use the repository's archive workflow when one exists; age, word count, and a target quota are not archive criteria.

The archive path is `archived/{class}/yyyy-mm-dd-topic-title.md`; `implemented` is deliberately absent because only implemented notes can enter it. An archival change moves the complete English/Chinese/sidecar triplet, retains `Status: implemented`, inserts the same `Archived: YYYY-MM-DD` line immediately below that status in both language files, re-records the sidecar, and repairs or deletes inbound links. These are the only permitted content changes during archival.

Once sealed, every archived triplet is permanently frozen. Do not edit, translate, reformat, update, move, or delete it, and do not treat it as authority for current behavior. A repository archive verifier should enforce the closed class tree, complete triplets, archive metadata, sidecar hashes, and an append-only content manifest.

## When to write one

Every non-trivial change MUST add or update at least one Agent Note in the same change. A change is non-trivial when it alters behavior, architecture, a contract shared across files, process or tooling, testing strategy, an on-disk, wire, or configuration format, or another decision a maintainer may reasonably revisit. A proposal for substantial future work starts in `proposed/`; a decision already made starts in `implemented/`.

Updating the note that already owns the decision satisfies the rule; do not create a duplicate. A note is never edited into a different decision: supersede it with a new note, and keep both notes cross-linked unless the old note is later fully consolidated.

An implemented note that is fully superseded may be consolidated into the current owning note and deleted. Before deletion, preserve every unique rationale, alternative, consequence, required verification, and named coverage gap; repair every inbound link; and delete the Chinese counterpart and consistency record in the same change. Partial supersession keeps both notes active and cross-linked.

A feature-addition note may be consolidated into a later removal note only when the feature is absent from production code, configuration, schemas, durable or wire formats, migration, compatibility behavior, current documentation, and supported tests. The removal owner preserves the original motivation, why it no longer justified the feature, alternatives to full removal, the capability given up, reintroduction conditions, and verification of complete absence.

## The file format

Every active Agent Note follows one in-file format. Archived notes retain the format they had when sealed plus the archive-date line above.

### The header block

The first three lines of every Agent Note are exactly:

```markdown
# Agent Note: <title>

Status: <status>
```

The status must agree with the lifecycle folder:

- `Status: proposed`
- `Status: implemented`
- `Status: rejected — <why, in one line>`

The status carries no dates or parentheticals. The filename holds the first-proposed date, and the body records amendments. A rejection reason is the one status form with content because it is the fact readers need immediately.

### The body skeleton

Every Agent Note opens with `## Problem`, which states the motivation without assuming the solution. Recurring sections use the canonical names below; genuinely bespoke technical sections may appear between the required sections.

#### `proposed/`

```markdown
## Problem
## Proposal
... bespoke sections ...
## Alternatives considered
## Acceptance criteria
## Risks
```

`## Proposal` may speak in the future tense. Plans, migration steps, and open questions belong there while the work is unbuilt. `## Acceptance criteria` says what observable state means done. `## Risks` covers what could go wrong and what the change knowingly gives up.

#### `implemented/`

```markdown
## Problem
## Decision
... bespoke sections ...
## Alternatives considered
## Consequences
```

`## Decision` describes shipped reality in the present tense, and the whole file stays current with it. Proposal-era headings such as `## Proposal`, `## Plan`, `## Migration plan`, and `## Acceptance criteria` do not belong in an implemented note. A `## Testing`, `## Deferred`, or `## Related` section is fine when it states present-tense fact.

#### `rejected/`

A rejected note is the proposal, frozen. It keeps its proposal-time sections, and the verdict lives on the `Status:` line. The header block, `## Problem`, `## Proposal`, and the alternatives mandate still apply.

### Alternatives considered - mandatory

Every Agent Note carries an `## Alternatives considered` section. Record each genuine alternative and why it lost, with one bold-led paragraph or a `### Why not <X>?` subsection per contested alternative. Alternatives are recorded, never invented.

For an older note whose alternatives cannot be reconstructed, keep this exact marker in place of the section when the repository's format policy permits it:

```markdown
<!-- agent-note-format: alternatives-not-recorded (pre-format Agent Note) -->
```

### Moving between lifecycles

Moving a file between lifecycle folders means updating the `Status:` line and satisfying the destination skeleton in the same change. A `proposed/` to `implemented/` move rewrites `## Proposal` into a present-tense `## Decision`, folds acceptance criteria and risks into `## Consequences` or present-tense verification sections, and removes plans in favor of what shipped. A `proposed/` to `rejected/` move adds the reason to the `Status:` line and freezes the file.

### Chinese counterparts

A `.zh.md` counterpart mirrors its English sibling section for section under the [i18n contract](../../docs/i18n/README.md). Machine-checked header tokens, including `# Agent Note: ` and `Status:`, stay in English verbatim. The format checker may skip the Chinese file; the pairing checker owns its consistency.
