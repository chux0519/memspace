# Explicit evidence relationships

This is an opt-in SQLite projection; `[evidence] mode = "off"` remains the
product default. `shadow` captures sources and traces direct relationships
without changing context output. `on` additionally groups validated sources.
No LLM or new relevance weights are used. Existing qualification remains intact.
`max_related_nodes = 24` bounds direct edges inspected per query, matching the
existing deduplicated candidate bound. Values 1..512 are resource bounds, not a
claim about the best recall setting. Zero selects 24. There is no graph walk.

Native reply relationships and actual generator `source_refs` are captured
when events/derived memories are written. A complete ordered import is labeled
`ordered_source`, distinct from `native_ref`. Timestamps never establish a reply.
Coarse generator provenance is available for inspection, but does not force an
entire session's sources into a prompt. Exact source bundles require explicit
retained spans. Relations never certify that a source or its answer is true.

## Structured source and correction inputs

Use existing authenticated `/api/v1/events/batch` ingestion and the usual
project/session/device envelope. Source documents use `source.document_captured`
with `content`, `url` and/or `path`, optional `git_commit`, `git_blob`,
`captured_at`, and `complete` (false means a retained excerpt). No URL is fetched.
The source version is SHA256 of canonical retained UTF-8 bytes (CRLF/CR to LF;
spaces and Unicode forms unchanged). An empty body is `locator_only`.

`evidence.link_asserted` has this payload contract:

```text
relation: derived_from | supersedes
basis: explicit_assertion | model_inference
from, to:
  event_id OR object_id
  revision_sha: exact source version
  span: {start, end, sha256}      # half-open canonical UTF-8 byte range
scope:
  entity, attribute, environment # mandatory for supersedes
  qualifiers: {branch: ..., version: ...}  # optional, exact conditions
witnesses: [endpoint, ...]       # 1..16 retained source/version references
```

Corrections require exact spans on both endpoints. Scope is an explicit
structured assertion by the emitting device; validation does not independently
prove the prose entails that assertion. Missing scope remains a candidate,
not a wildcard. `model_inference` always remains a candidate; clients cannot
claim native/generator authority. Self links, wrong hashes or UTF-8 boundaries
are invalid. References unavailable in the same project remain pending.
Native source changes do not silently repair an invalid span after redaction.

Assertions arriving before endpoints are kept in `evidence_inputs`, with status
and diagnostics. Each arriving event/object retries up to 256 matching pending
inputs; this is an ingestion work bound, not a confidence threshold. Massive
fan-in beyond that bound requires replaying the pending assertion events.
`evidence.link_retracted` names `assertion_event_id`; its scoped tombstone works
before the assertion arrives. Independent active observations are retained.
Competing or cyclic corrections in the same scope become candidates. Resolving
a conflict does not silently promote candidates: retract incorrect observations
and submit a new explicit assertion. All original and frozen versions remain.

## Query and inspection

`on` forms source bundles before the existing prompt/reply role suppression.
The independently admitted anchor keeps its original content window and rank.
Required companion endpoints/witnesses consume real source slots, and session
diversity is preserved. A group is either delivered or removed as a whole by
exclusions, item count and rendered character budget. A long/partial source is
marked incomplete. Exact retained spans may be complete even if the original
article is only retained in part; `availability=excerpt` stays visible.
Corrections are labeled with their exact scope and dates; old values are never
silently removed, so the answering agent can handle historical/as-of questions.
No rule assumes "newest is true". Candidate/retracted/invalid links do not pack.

The context JSON includes `evidence_groups` only for delivered groups, with
source/version IDs, required spans, relation basis/state and completeness.
`complete` concerns the required retained sources, never all evidence for an
answer. `GET /api/v1/evidence/{object_id}?project_id=...&limit=24` requires the
usual read authentication and returns the retained source, document locator,
version, and direct links including inactive states. It does not traverse into
other projects. `GET /api/v1/evidence/assertions/{event_id}?project_id=...`
returns an assertion's pending/invalid/candidate/active/retracted status and diagnostic.
Article content hashes do not prove a live URL is unchanged.

## Measurement limitations

Default-off, ownership/version/idempotency/budget checks and shadow parity are
covered by tests. The private real-library backfill validates preservation and
native coverage; it is keyword-only and has no answer quality labels. New
relations are experimental: no quality improvement is claimed until the same
frozen dataset, answer/judge models, budgets and per-question protocol are run.
Frozen old bodies absent before capture cannot be recovered. Current derived
source versions are validated against actual memory/summary content/provenance.
This does not automatically extract every correction from natural-language
history or infer which source sentence supports each summary sentence.
