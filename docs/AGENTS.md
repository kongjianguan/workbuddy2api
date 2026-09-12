# AGENTS.md - Documentation Standard

This file defines document placement, documentation tiers, writing rules, and any repository-specific documentation budgets. Use the repository's own validation commands when present; this file owns documentation boundaries and prose, not implementation-specific checks.

## Document structure

These rules apply to human-facing documentation. Agent Notes remain under `.agents/notes/` and use their own lifecycle and file-format rules. A document's subject and tree position determine its scope: describe the subject at the detail appropriate to that position, and link to the owning descendant for lower-level facts.

Classify each document as a tutorial or a reference. Tutorials lead a reader through an ordered path to an observable outcome. References define a lookup scope and current behavior without a teaching sequence. Separate substantial tutorial and reference material; label a section when either part is small.

Before writing a tutorial, classify the reader's starting knowledge and each concept as beginner, intermediate, or advanced. Establish prerequisites before dependent concepts, increase difficulty gradually, and move unnecessary advanced material to a later tutorial or reference.

Author in this order: locate the document in the tree; set its permitted detail; choose tutorial or reference; order tutorial concepts by prerequisite and difficulty; relocate descendant-owned detail; replace lower-level explanations with links to their owners.

## The tier taxonomy: one home per fact

Each fact has one home: the tier whose job is to own it. Elsewhere, link to that home.

| Tier | Job | Does NOT belong there |
|---|---|---|
| Root `AGENTS.md` | Standing instructions needed in every session, with links to their homes | Stories, worked examples, situational procedures, or duplicated policy |
| Subtree `AGENTS.md` | Instructions specific to one directory | Repository-wide rules already owned by the root file |
| Architecture reference | Composition, boundaries, lifecycles, dependencies, and extension points | Type catalogs, package detail, decision rationale, or status annotations |
| Subsystem reference | Types, semantics, invariants, public entry points, and failure behavior for one subsystem | Repository-wide behavior narration |
| Agent Notes | Decisions, alternatives, trade-offs, and required verification | Current reference facts, migration checklists, or shipped plans |
| Postmortems | Evidence and guardrails for subtle, systemic failures | Ordinary feature design or routine bug descriptions |
| Cookbook | Step-by-step procedures with observable verification | Design rationale or exhaustive reference tables |
| User documentation | Product-facing guides and task-oriented explanations | Contributor procedures, generated catalogs, or decision history |
| Component README | The component's contract, configuration, limitations, and extension points | JSDoc restatement or other components' concerns |
| Generated reference | Exhaustive facts regenerated from source or a generator | Hand edits to generated output |
| Skills and agent instructions | Reusable workflows and standing orders | Product or runtime contracts that belong in source or docs |

Placement follows purpose: incidents belong in postmortems; rationale belongs in Agent Notes; procedures belong in cookbooks; type and subsystem contracts belong in subsystem references; component contracts belong in component READMEs; standing orders belong in the nearest applicable `AGENTS.md`.

## Writing rules

- **Document current state, not change history.** Avoid narrated transitions, review chronology, branch position, and commit history in durable prose. Put history in commits, decision records, or postmortems when it is needed as evidence.
- **Every non-trivial change includes an Agent Note.** Update the note that already owns the decision or add one in the same change. A purely mechanical or local edit is the narrow exception.
- **Keep one physical line per paragraph** when the repository's Markdown checks require it. Preserve code blocks, tables, and list structure.
- **Keep source-derived facts source-owned.** Link to a declaration or regenerate a reference instead of maintaining a second signature, option, or catalog by hand.
- **Keep bilingual pairs together.** Update both languages and the consistency record in the same change when the repository has an i18n contract.
- **Comments and JSDoc state complete contracts.** Preserve behavior, failure, timing, ownership, modality, exceptions, and non-obvious orientation. Remove implementation narration, test walkthroughs, review analysis, and code restatement.
- **Write directly.** Name the actor, fact, boundary, operation, and verification. Use normative keywords only when their enforcement strength matters.
- **Use links for ownership.** Do not repeat a rule or catalog in several homes merely to make a page feel complete.

## Documentation budgets

If the repository has a documentation budget manifest, treat its ceilings as guardrails. When a document exceeds its ceiling, first relocate content to its owning tier, then condense duplicated or unnecessary prose. Raise a ceiling only when the document needs the space and the manifest change records why. A budget is not a target for deleting useful detail.

## The slop checklist

Before finalizing a document, check for:

- the same rule stated in more than one home;
- narrated history or implementation-status labels in current reference prose;
- hand-maintained catalogs where source or a generator is authoritative;
- reasoning transcripts, obvious branch walkthroughs, or rejected local alternatives;
- rationale repeated beside sibling APIs instead of at the owning capability;
- paragraphs carrying several unrelated rules and parenthetical asides;
- excessive bold, capitals, or warning language;
- proposal language, migration plans, or acceptance checklists left in an implemented Agent Note.

## Cross-references

Use relative Markdown links for repository references. Link to the owning document, source declaration, generator, or decision record instead of naming it only in prose. Run the repository's link checker after moving or removing a document.
