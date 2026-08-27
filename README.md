# Stuff

**Status:** tested MVP

Stuff is a small durable store for named Items, Notes about those Items, and optional documentation describing recurring metadata conventions.

It is intended for humans and agents who need to leave behind enough structured context that later participants can discover what exists, understand how prior participants represented it, query it, and continue the work.

Stuff is not a task manager or execution engine.

## Running the MVP

Stuff is one Go binary. The CLI talks to the HTTP service; the service is the only
component that receives CouchDB credentials.

```bash
nix develop                         # Go, CouchDB, and development tools
nix build                           # produces result/bin/stuff

export STUFF_COUCH_URL=http://127.0.0.1:5984
export STUFF_COUCH_DB=stuff
export STUFF_COUCH_USER=stuff
export STUFF_COUCH_PASSWORD_FILE=/run/credentials/stuff/couchdb-password
export STUFF_TOKEN_FILE=/run/credentials/stuff/api-token # or use STUFF_TOKEN
stuff serve
```

`STUFF_LISTEN` defaults to `127.0.0.1:7847`. Clients use `STUFF_URL` and
`STUFF_TOKEN`. A CouchDB URL containing credentials is also accepted for local
development, but password files are preferred for deployment. The service creates its
database if necessary. CouchDB remains loopback-only on Azula; Stuff is the bounded
gateway.

The MVP stores attachments using CouchDB attachment bodies, returns only descriptors in
ordinary Note JSON, and forces downloads through a sandboxing CSP and attachment content
disposition. A separate document origin remains the intended production boundary for
active HTML.

Verification includes unit/HTTP contract tests and a live CouchDB integration covering
Items, point-in-time validation and drift, Notes, attachment round-tripping, Mango
projection and selectors, explain, and describe.

## Thesis

> Stuff stores Items and Notes. Some metadata conventions may be documented as Schemas. Everything else is interpretation performed by clients.

An Item is a durable namespace for a concern: a branch node on a mind map, not a work-tracking form whose fields imply a prescribed lifecycle.

An Item might be narrow:

- Fix the stale sidebar color assertion
- Find my keys
- Investigate mobile voice playback cutoff

Or broad:

- Familiar
- Fort Nix
- Home

There is no separate Project, Epic, Quest, or Milestone type. Those are Items whose metadata and relationships a client happens to find meaningful.

A Note is an inert statement made about an Item. It may contain text, arbitrary metadata, and attachments. A Note can describe an attempted fix, record a decision, hold a runbook, attach a report, or say "I tried this and it went badly." Those interpretations do not create behavioral subtypes.

## Vocabulary

- **Stuff** — the product and CLI.
- **Item** — a named durable namespace with arbitrary metadata.
- **Note** — a timestamped statement associated with an Item.
- **Schema** — optional documentation for a recurring metadata convention, with validation available when explicitly requested.

The vocabulary is deliberately plain. In particular:

- an Item is not necessarily actionable or completable;
- a Note is not an event in a workflow state machine;
- a Schema is not an enforced database constraint.

## Minimum primitives

### Item

An Item has a small system-owned envelope and caller-owned metadata.

```json
{
  "id": "item_01J6A9…",
  "name": "Mobile voice simplification and hardening",
  "created_at": "2026-08-26T22:00:00.000Z",
  "updated_at": "2026-08-26T22:11:43.219Z",
  "revision": "3-9e28…",
  "metadata": {
    "area": "familiar",
    "parent_item_id": "item_familiar",
    "state": "open",
    "next_action": "Capture playback lifecycle diagnostics"
  }
}
```

System fields:

- `id` — stable opaque identifier. Ordering uses timestamps, never ID shape.
- `name` — human- and model-legible name for the namespace.
- `created_at` — server timestamp.
- `updated_at` — server timestamp.
- `revision` — optimistic edit token preventing silent lost updates. It is not a work lock or lease.
- `metadata` — arbitrary bounded JSON owned by callers.

Stuff does not intrinsically define:

- status;
- priority;
- completion;
- ownership;
- deadlines;
- parents or children;
- projects or epics;
- dependencies.

Clients may establish any of those by convention in `metadata`.

### Note

A Note says something about one Item.

```json
{
  "id": "note_01J6AB…",
  "item_id": "item_01J6A9…",
  "created_at": "2026-08-26T23:02:18.440Z",
  "updated_at": "2026-08-26T23:02:18.440Z",
  "revision": "1-761b…",
  "text": "Heh, yeah, I tried fixing this bug. Lemme tell ya about it.",
  "metadata": {
    "kind": "attempt",
    "external_system": "golem",
    "external_id": "job-b04b…",
    "started_at": "2026-08-26T22:31:00Z",
    "finished_at": "2026-08-26T23:01:40Z",
    "outcome": "mixed"
  },
  "attachments": []
}
```

A Note has:

- a stable ID and revision;
- exactly one associated Item;
- optional text;
- arbitrary bounded metadata;
- zero or more attachments.

Words such as `attempt`, `decision`, `report`, `runbook`, and `observation` are useful metadata conventions. They are not enforced Note classes.

A Golem dispatch may be recorded as a Note containing an external ID, timestamps, and a narrative. Stuff does not ask whether the Golem still exists, synchronize its state, retry it, or treat that Note as authoritative execution state.

### Attachments

Documents are Notes with attachments, not a separate domain class.

```json
{
  "item_id": "item_presence",
  "text": "Presence lifecycle design",
  "metadata": {
    "kind": "document",
    "role": "design"
  },
  "attachments": [
    {
      "name": "presence-lifecycle.html",
      "media_type": "text/html",
      "bytes": 18422,
      "sha256": "d98c…",
      "url": "https://documents.example/note_01J6AC/presence-lifecycle.html"
    }
  ]
}
```

Each independently changing document should generally have its own Note. This gives it independent provenance and revision history and avoids mutating an Item whenever an attachment changes.

Large payloads may eventually live in content-addressed object storage while Stuff retains attachment metadata and stable retrieval URLs.

Arbitrary HTML must be served from an isolated document origin with appropriate content security policy and sandboxing. It must not execute on the authenticated application origin.

## Schemas: documentation with an optional validator

A Schema documents how somebody previously decided to represent a recurring shape of metadata.

It is analogous to `DESCRIBE TABLES`, not a database integrity constraint.

Schemas should use standard JSON Schema rather than a novel type language. Schema writes are rare; interoperability and discoverability matter more than concision.

```bash
stuff schema add todo @todo.schema.json
stuff schemas
stuff schema get todo
```

Example schema:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["project", "priority"],
  "properties": {
    "project": { "type": "string" },
    "priority": { "type": "number" }
  },
  "additionalProperties": true
}
```

Validation is explicitly requested and applies to that operation or question.

```bash
stuff schema check todo --meta @candidate.json
stuff schema check todo item_123
```

Write-time convenience:

```bash
stuff add "Find my keys" \
  --meta '{"project":"home","priority":5}' \
  --validate todo
# item_…
```

```bash
stuff add "Find my keys" \
  --meta '{"priority":5}' \
  --validate todo
# error: metadata.project is required by schema "todo"
```

An Item may later drift out of conformance:

```bash
stuff update item_123 --meta '{"location":"probably the garage"}'
# accepted
```

A caller may request validation during an update:

```bash
stuff update item_123 \
  --meta '{"project":"home","priority":3}' \
  --validate todo
```

Successful validation is a point-in-time result. It does not attach the Schema to the Item or create an ongoing conformance promise.

Schema changes do not scan, migrate, reject, or invalidate existing Items. Stuff does not promise that an Item previously checked against `todo` still satisfies `todo`.

The Schema means:

> Someone intended this metadata convention to look like this. Here is their documentation. Check it when useful.

Updating a Schema updates the documentation. Historical definitions may remain available through ordinary revision history, but Stuff does not create a migration subsystem.

Agents may inspect Schemas and derive Mango selectors using operators such as `$exists`, `$type`, `$and`, and `$not` to find records that do or do not resemble a convention.

## Query language

Stuff uses full CouchDB Mango selector semantics rather than inventing a subset.

A subset would create a new language agents must discover through failure. Full Mango is documented, represented in model training data, and implemented by the likely storage substrate.

Example:

```bash
stuff find <<'JSON'
{
  "selector": {
    "metadata.area": "familiar",
    "metadata.state": "open"
  },
  "fields": [
    "id",
    "name",
    "metadata.next_action",
    "updated_at"
  ],
  "sort": [{"updated_at": "desc"}],
  "limit": 50
}
JSON
```

Query envelope:

```json
{
  "selector": {},
  "fields": [],
  "sort": [],
  "limit": 50,
  "bookmark": "opaque-token-from-prior-page"
}
```

Expected Mango selector vocabulary includes:

- implicit equality and AND;
- `$eq`, `$ne`, `$gt`, `$gte`, `$lt`, `$lte`;
- `$exists`, `$type`;
- `$in`, `$nin`, `$all`, `$size`, `$elemMatch`, `$allMatch`;
- `$and`, `$or`, `$not`, `$nor`;
- `$beginsWith`, `$regex`, `$mod`;
- `$keyMapMatch`;
- `$text` only when a compatible text-search index exists.

Full Mango does not imply unbounded resource use. The service may cap:

- selector depth and encoded request size;
- page size;
- execution time;
- regex length;
- returned bytes.

Those are resource policies, not language differences.

At the expected scale, fallback scans are acceptable. Stuff should expose warnings when no useful index exists rather than rejecting the query. Repeated real query shapes can later justify indexes along demonstrated desire paths.

## Agent interface: a CLI, not a forest of tools

The canonical agent interface is the `stuff` CLI invoked through an ordinary shell execution tool.

Agents are already fluent in shell composition. A single Bash call containing a short script is cheaper, clearer, and more expressive than many custom CRUD tool turns.

```bash
d=$(stuff add "Do the dishes") &&
  stuff note add "$d" "Use the powdered detergent"
```

Another example:

```bash
item=$(stuff add "Mobile voice hardening" \
  --meta '{"area":"familiar","state":"open"}') &&

stuff note add "$item" \
  "Assistant playback stopped during a driving voice turn." \
  --meta '{"kind":"observation"}'
```

Attach a report:

```bash
stuff note add "$item" \
  "Playback lifecycle investigation" \
  --meta '{"kind":"report"}' \
  --attach report.html
```

Query recent Notes:

```bash
stuff note find <<JSON
{
  "selector": {
    "item_id": "$item",
    "created_at": {"\$gte": "2026-08-20T00:00:00Z"}
  },
  "sort": [{"created_at":"desc"}],
  "limit": 20
}
JSON
```

### CLI output contract

Shell composition makes output discipline load-bearing.

- Successful create operations write only the new ID to stdout by default.
- Diagnostics and warnings go to stderr.
- Failures return nonzero.
- Retrieval and query operations return stable JSON.
- Large bodies and queries may be supplied through stdin or files.
- Machine behavior must not change merely because stdout is a TTY.
- Human-oriented formatting is explicit, for example `--pretty`.
- Large attachment bodies are never dumped into ordinary list/query output.

The CLI may communicate with a local or remote Stuff service. The service transport is an implementation detail; agents should not need to manage raw CouchDB requests, credentials, or revision fields unnecessarily.

## Discoverability

There are two discovery problems:

1. learning the query language;
2. learning the metadata conventions used by this particular store.

Full Mango addresses the first. Stuff must explicitly support the second.

```bash
stuff --help
stuff find --help
stuff schemas
stuff describe
stuff explain @query.json
```

`stuff describe` should report bounded information such as:

- Item and Note envelopes;
- supported Mango version and operators;
- query and payload limits;
- observed metadata paths;
- observed JSON types and presence counts;
- bounded example values;
- common Note metadata conventions such as `kind`;
- available indexes;
- whether text search is configured;
- executable examples using vocabulary present in the current store.

Example:

```json
{
  "items": 84,
  "observed_fields": [
    {
      "path": "metadata.area",
      "types": ["string"],
      "present": 71,
      "examples": ["familiar", "golem", "fort-nix"]
    },
    {
      "path": "metadata.state",
      "types": ["string"],
      "present": 52,
      "examples": ["open", "done", "deferred"]
    },
    {
      "path": "metadata.parent_item_id",
      "types": ["string"],
      "present": 38
    }
  ],
  "mango_reference": "CouchDB 3.5 Mango selectors",
  "limits": {
    "limit_max": 200,
    "selector_bytes_max": 65536
  }
}
```

This is not an inferred schema registry. It is a map of footprints in the snow.

Errors should be corrective. A malformed query or failed validation should include:

- the failing JSON path;
- the original reason;
- the expected shape or nearest valid form;
- a small corrected example where practical.

An agent should normally repair an error in one attempt.

## Storage recommendation

CouchDB is a strong initial substrate because it already provides:

- arbitrary JSON documents;
- Mango querying, projection, sorting, bookmarks, and explain;
- stable IDs and optimistic revisions;
- attachments with MIME types and range requests;
- a changes feed;
- acceptable fallback scans at small scale;
- replication if it is ever actually needed.

Stuff should expose a thin gateway rather than raw CouchDB administration or credentials. The gateway owns:

- public Item, Note, Schema, and attachment contracts;
- server timestamps;
- payload and query limits;
- stable document URLs;
- authentication;
- `describe` and corrective errors;
- isolation of active attachment content.

The gateway should preserve full Mango selector behavior. It should not emulate a novel partial version of Mango.

SQLite with JSON/JSONB remains a credible future substrate. It is not the recommended first implementation because accepting full Mango over SQLite would require building and testing a query compiler, selector edge cases, collation, pagination, explain behavior, and error compatibility before the primitive has earned that work.

## Hard scope boundaries

Stuff owns:

- Item identity and names;
- arbitrary bounded Item metadata;
- Notes and their arbitrary bounded metadata;
- attachments;
- server timestamps and revisions;
- advisory Schemas;
- explicitly requested validation;
- full Mango querying and pagination;
- introspection and corrective errors;
- stable attachment retrieval.

Stuff does **not** own:

- execution or dispatch;
- attempts or retries;
- workers or worker recovery;
- locks, leases, or duplicate-execution prevention;
- scheduling;
- reconciliation with external systems;
- worklist or notification semantics;
- status or priority enums;
- projects, epics, quests, or milestones;
- dependency enforcement or DAG validity;
- automatic handoff or surfacing policy;
- schema migrations or continual schema enforcement.

Clients may implement any of those using queries and conventions. That does not move the behavior into Stuff. Stuff may record that an execution occurred; that does not make Stuff an execution engine.

## Provisional CLI surface

Names may change, but the first implementation should remain close to:

```text
stuff add NAME [--meta JSON|@FILE] [--validate SCHEMA]
stuff get ITEM
stuff update ITEM [--name NAME] [--meta JSON|@FILE] [--revision REV] [--validate SCHEMA]
stuff find [@QUERY | stdin]

stuff note add ITEM [TEXT] [--meta JSON|@FILE] [--attach FILE ...]
stuff note get NOTE
stuff note find [@QUERY | stdin]

stuff schema add NAME @SCHEMA
stuff schema get NAME
stuff schema check NAME ITEM
stuff schema check NAME --meta JSON|@FILE
stuff schemas

stuff describe
stuff explain [@QUERY | stdin]
```

Revision checks prevent accidental lost edits. They are not locks on work.

## Suggested first proof

1. Stand up a local Stuff service backed by CouchDB.
2. Implement the minimum CLI create/get/update/find paths for Items and Notes.
3. Add full Mango passthrough with bounded limits and useful errors.
4. Add Schema storage, listing, and explicit JSON Schema validation.
5. Migrate the Azula first-day ledger into Items and Notes.
6. Give a fresh agent only `stuff --help` and access to the CLI.
7. Ask it to discover the active Familiar concerns and retrieve the relevant history.

The proof succeeds if the agent can orient itself with `stuff describe`, `stuff schemas`, and one or two Mango queries—without being taught a private query language and without Stuff becoming responsible for executing any of the work it describes.
