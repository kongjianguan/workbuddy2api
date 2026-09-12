# Postmortems


A postmortem records a failure that reached a user, an integration, a release, or another meaningful boundary. The useful subject is why the process allowed it through, not only the one-line fix.

A postmortem is not an [Agent Note](../../.agents/notes/README.md). An Agent Note records a deliberate design decision, its rejected alternatives, or a future proposal. A postmortem is a backward-looking record of what broke, the mechanism, why existing safeguards missed it, and the concrete guardrails added so the same class of failure becomes visible earlier next time.

Write one when the failure is **subtle**, **systemic**, and **costly to rediscover**. Link the tests, instructions, decision records, or operational safeguards that the postmortem motivated.

Every postmortem opens with an **Executive summary**: one short paragraph that states what broke, the root cause in plain terms, why it escaped, and the durable lesson. Follow it with the detailed evidence needed to understand and prevent recurrence.

| # | Title |
|---|---|
<!-- Add one row per real postmortem. -->
