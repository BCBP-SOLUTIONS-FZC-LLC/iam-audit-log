---
name: Feature request
about: Propose a new taxonomy entry, endpoint, consumed event, or service behaviour
title: '[FEAT] '
labels: enhancement
assignees: ''
---

## Problem / motivation
What problem does this solve? Which producers, tenant admins, or compliance use cases are affected?

## Proposed solution
Describe the API or behaviour change you'd like. Cite the LLD section it refines, or note that an LLD change is required first.

```go
// Example: new entry_type row, request field, or query filter
```

## Affected areas
- [ ] Taxonomy (new `entry_type` / retention tier — AL-INV-6 monotonic-up only)
- [ ] Query / export API
- [ ] Direct-write ingest contract (producers must be notified)
- [ ] Consumed event (new queue/topic — asyncapi.yaml update needed)
- [ ] Database schema (new migration — production-data-migrations gate)
- [ ] Archival / retention
- [ ] Helm / deployment config

## Retention / compliance impact
Which retention tier would the new records land in, and why (AL-D2)?

## Alternatives considered
Other approaches you evaluated and why you ruled them out.

## Acceptance criteria
- [ ]
- [ ]
- [ ]

## Additional context
Links to related issues, HLD / LLD sections, or decision-register entries (AL-D*).
