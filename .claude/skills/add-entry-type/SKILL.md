---
name: add-entry-type
description: Add a new entry_type to the taxonomy — updates taxonomy.go, asyncapi.yaml (bus types only), LLD §7.1/§25, and taxonomy_test.go in the correct order per CLAUDE.md conventions.
disable-model-invocation: true
---

# Add Entry Type

Walk through adding a new entry_type end-to-end. Ask for the following before starting:

1. **Source** — bus topic (which one?) or direct-write (AL-5)?
2. **source_event_type** — the event name on the wire (e.g. `TenantSeatsChanged`)
3. **entry_type** — the frozen name in our convention (e.g. `tenant.seats_changed`)
4. **Retention tier** — `security_3y`, `access_90d`, or `compliance_7y`
5. **Producer restriction?** — only for direct-write; name the allowed `source_service` values (leave blank for the D-6 default: any mesh-admitted caller)

Then make the changes in this order:

**Step 1 — `internal/core/domain/taxonomy.go`**
- Bus type: add to `busTypes` map as `{TopicX, "EventName"}: "entry.type"`
- Direct-write: add to `entryTypes` map with `{tier: TierX, directWrite: true}`
- Producer-restricted: also add a `producers` slice and reference it in the entry

**Step 2 — `api/asyncapi.yaml`** (bus types only)
- Add the message definition under `components.messages`
- Add the message ref to the channel's `messages` map
- Add the message ref to the `receiveX` operation's `messages` list
- Add a payload schema under `components.schemas`

**Step 3 — `internal/eventschema/<snake_case_event>.json`** (bus types only)
- Add the JSON schema file extracted from the AsyncAPI payload definition

**Step 4 — `docs/lld/iam-lld-audit-log-service.md`**
- Add a §7.1 table row
- Add the entry_type to the §25 frozen-vocabulary line (note the rev)
- Add a revision table entry at the top of the revision log

**Step 5 — `internal/core/domain/taxonomy_test.go`**
- Bus: add a row to `lldTable`
- Direct-write: add to `lldDirectWrite` map
- Update the vocabulary count assertion (`len(EntryTypes()) != N`)

**Step 6 — verify**
Run `make vet lint arch-lint invariant-lint` before committing.
