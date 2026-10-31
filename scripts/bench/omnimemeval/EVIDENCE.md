# Optional evidence capture

`[evidence] mode = "off"` is the default. `shadow` captures native message/turn
references and the production generator's actual `source_refs`, preserving
context output. `on` is reserved for the relation packing stage; do not use it
as a claim of measured quality before its own validation. `max_related_nodes`
defaults to 24 (the existing candidate hydration bound); values 1..512 are an
engineering resource limit, not a fitted relevance threshold. Zero uses 24.

The worker accepts `MEMSPACE_EVIDENCE_MODE=off|shadow|on`; an evaluation root's
mode is immutable. Original events and frozen derived versions are authoritative.
Stable object IDs include project, source ID and canonical version hash, never
search row numbers. Canonicalization only normalizes CRLF/CR to LF.

`reply_to` is supported by unique native parent/turn/prompt IDs scoped to the
same project, session and device. Missing parents remain unresolved; ambiguous
parents are candidates. Timestamps and similarity never establish a parent.
Complete imported source batches may establish an `ordered_source` relationship,
which is labeled separately from a native reference. Partial batches do not.

`derived_from` records declared generator provenance. It does not prove every
source supports every sentence. Source sessions are retained in the frozen
snapshot, without expanding an entire session. Missing historical versions are
not reconstructed from current text. Current memory/summary data are checked
against their frozen revision before reuse, including provenance changes.

Offline backfill is explicit and restricted to a disposable backup:

```bash
go run -tags sqlite_fts5 ./scripts/bench/evidence -db /private/copy.db -out /private/new-run
```

The command captures stored records without regenerating summaries, checks the
original event digest, and compares 50 keyword-only packs before/after shadow.
Its private manifest reports counts and limitations. It is not a retrieval
quality score and never opens the production database or calls a model.
