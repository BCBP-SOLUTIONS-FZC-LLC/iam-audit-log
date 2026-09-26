---
name: Bug report
about: Report a defect in the iam-audit-log service (query/ingest API, SQS consumers, reconciler, or database layer)
title: '[BUG] '
labels: bug
assignees: ''
---

## Description
A clear description of the bug.

## Service version
`github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log` — tag / commit SHA:

## Go version
`go version goX.Y.Z ...`

## Environment
- [ ] Local dev (`make run`)
- [ ] Docker Compose (`make docker-up`)
- [ ] Staging
- [ ] Production

## Affected area
- [ ] Query / export API (AL-1..AL-4, endpoint: `METHOD /api/v1/audit/...`)
- [ ] Direct-write ingest (AL-5/AL-6)
- [ ] Internal read (AL-7)
- [ ] SQS consumer (queue: `*-audit-q`)
- [ ] Reconciler (archive / verify / drop / prune — job: `--job=...`)
- [ ] Redaction pipeline
- [ ] CAT-I2 plans poller
- [ ] Database / migrations / RLS / grants

## Compliance impact
Could this lose, mutate, mis-attribute, or leak an audit record? If yes, also follow RB-1 / RB-5.

## Steps to reproduce
1.
2.
3.

## Expected behaviour
What you expected to happen.

## Actual behaviour
What actually happened. Include error codes (§17), HTTP status codes, log output, or stack traces.

```
// paste relevant log output or error here
```

## Minimal reproduction
```go
// paste the smallest snippet or curl command that triggers the bug
```

## Additional context
Any other relevant context (Postgres version, PgBouncer mode, queue/DLQ depth, related issues).
