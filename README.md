# Stuff

Stuff is a small durable store for named Items, Notes about those Items, and optional JSON Schemas that document recurring metadata conventions.

It is designed for humans, scripts, and agents that need to leave behind structured context without adopting a task manager or workflow engine.

> Stuff stores Items and Notes. Schemas document optional conventions. Everything else is client interpretation.

## Status

Stuff is an early, tested MVP. The CLI and HTTP service are provided by one Go binary, backed by CouchDB.

## Why Stuff?

Most work-tracking systems begin by prescribing a lifecycle: projects contain tasks, tasks have statuses and priorities, dependencies form a graph, and workers move records through a workflow.

Stuff deliberately does less.

- An **Item** is a durable namespace for any concern.
- A **Note** is an inert statement associated with one Item.
- A **Schema** is optional documentation for a metadata convention.
- **Metadata** is arbitrary bounded JSON owned by clients.
- **Queries** use CouchDB Mango directly rather than a private query language.

There are no intrinsic projects, epics, statuses, priorities, owners, deadlines, dependencies, workers, retries, schedules, or execution semantics.

## Installation

### Nix

```bash
nix profile install github:gisikw/stuff
stuff --help
```

Or run it without installing:

```bash
nix run github:gisikw/stuff -- --help
```

### Go

Go 1.24 or newer is required.

```bash
go install github.com/gisikw/stuff/cmd/stuff@latest
```

## Quick start

Stuff requires CouchDB 3.x. This example starts a development instance with Docker:

```bash
docker run -d --name stuff-couch \
  -p 127.0.0.1:5984:5984 \
  -e COUCHDB_USER=stuff \
  -e COUCHDB_PASSWORD=development-only \
  couchdb:3
```

Start the Stuff service:

```bash
export STUFF_COUCH_URL=http://stuff:development-only@127.0.0.1:5984
export STUFF_COUCH_DB=stuff
export STUFF_TOKEN=development-api-token
stuff serve
```

In another shell:

```bash
export STUFF_URL=http://127.0.0.1:7847
export STUFF_TOKEN=development-api-token

item=$(stuff add "Evaluate a database migration" \
  --meta '{"area":"infrastructure","state":"open"}')

stuff note add "$item" \
  "The dry run completed without data loss." \
  --meta '{"kind":"observation"}'

stuff get "$item" --pretty
```

Create commands print only the new ID, making shell composition reliable.

## Browser reading surface

`stuff serve` includes a read-only browser surface:

- `/read` lists Items by effective activity, including activity from linked Notes.
- `/read/items/<item-id>` shows an Item, its metadata, Notes, and attachment downloads.

The activity view deliberately reads a bounded sample of at most 200 Items and 200 Notes, then paginates that sample locally. It displays a warning when either bound is reached. Note text uses a conservative Markdown renderer that treats stored HTML as inert text. Attachments retain forced-download disposition, a sandboxing content security policy, and `nosniff`; active HTML is never rendered in the application origin.

The `/read` routes are unauthenticated at the application layer so deployments can put them behind an identity-aware reverse proxy without exposing the API bearer token to a browser. Deployments **must** bind Stuff to a trusted interface or identity-gate these routes at the proxy. All `/v1` data and mutation routes remain protected by `STUFF_TOKEN` when configured.

## Data model

### Item

```json
{
  "id": "item_…",
  "name": "Evaluate a database migration",
  "created_at": "2026-08-27T03:00:00Z",
  "updated_at": "2026-08-27T03:10:00Z",
  "revision": "2-…",
  "metadata": {
    "area": "infrastructure",
    "state": "open"
  }
}
```

The system owns the ID, timestamps, and optimistic revision. The caller owns the name and metadata. Metadata may be any JSON value, although objects are usually the most queryable convention.

### Note

```json
{
  "id": "note_…",
  "item_id": "item_…",
  "created_at": "2026-08-27T03:12:00Z",
  "updated_at": "2026-08-27T03:12:00Z",
  "revision": "1-…",
  "text": "The dry run completed without data loss.",
  "metadata": {
    "kind": "observation"
  },
  "attachments": []
}
```

A Note belongs to exactly one Item. Terms such as `decision`, `attempt`, `report`, `runbook`, and `observation` are metadata conventions, not behavioral subtypes.

### Attachments

Documents are Notes with attachments:

```bash
stuff note add "$item" "Migration report" \
  --meta '{"kind":"report"}' \
  --attach report.html \
  --attach timings.csv
```

Ordinary Note retrieval and queries return attachment descriptors, never inline bodies. Attachment downloads use a restrictive content security policy, `nosniff`, and attachment disposition. Deployments that intentionally render active HTML should use a separate document origin.

## Advisory Schemas

Schemas use standard JSON Schema. They document a convention and validate metadata only when explicitly requested.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["area", "impact"],
  "properties": {
    "area": { "type": "string" },
    "impact": { "type": "number" }
  },
  "additionalProperties": true
}
```

```bash
stuff schema add assessment @assessment.schema.json

item=$(stuff add "Assess cache behavior" \
  --meta '{"area":"performance","impact":4}' \
  --validate assessment)

stuff schema check assessment "$item"
stuff schema check assessment --meta '{"area":"reliability","impact":5}'
```

Validation is a point-in-time question, not an enduring promise. A later write without `--validate` may drift from the convention and is accepted:

```bash
stuff update "$item" --meta '{"notes":"convention intentionally changed"}'
```

Successful validation does not attach a Schema to an Item. Updating a Schema does not scan, migrate, or invalidate existing records. External schema resources are not fetched during compilation; validation is offline.

## Mango queries

`stuff find` and `stuff note find` accept full CouchDB Mango query envelopes.

```bash
stuff find <<'JSON'
{
  "selector": {
    "metadata.area": "infrastructure",
    "updated_at": {"$gte": "2026-01-01T00:00:00Z"}
  },
  "fields": ["id", "name", "metadata.state", "updated_at"],
  "sort": [{"updated_at": "desc"}],
  "limit": 50
}
JSON
```

Stuff preserves Mango selectors, projections, sorts, bookmarks, warnings, and explain behavior while adding the Item or Note type boundary. Resource limits constrain request and response size; they do not replace Mango with a subset.

```bash
stuff explain @query.json --pretty
stuff describe --pretty
```

`stuff describe` reports envelopes, supported operators, limits, observed metadata paths and types, bounded examples, available indexes, and text-search availability. It is a map of observed conventions, not inferred enforcement.

## CLI reference

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
stuff serve
```

Output behavior is intentionally shell-safe:

- creates print only the new ID to stdout;
- diagnostics go to stderr;
- failures return nonzero;
- retrieval and query commands emit stable compact JSON;
- `--pretty` explicitly enables indented output;
- behavior does not change because stdout is a terminal;
- JSON and queries may be supplied inline, from `@FILE`, or from stdin where documented.

## Configuration

### CLI

| Variable | Default | Description |
| --- | --- | --- |
| `STUFF_URL` | `http://127.0.0.1:7847` | Stuff service URL |
| `STUFF_TOKEN` | empty | Bearer token |
| `STUFF_TOKEN_FILE` | empty | File containing the bearer token |

### Service

| Variable | Default | Description |
| --- | --- | --- |
| `STUFF_LISTEN` | `127.0.0.1:7847` | HTTP listen address |
| `STUFF_COUCH_URL` | `http://127.0.0.1:5984` | CouchDB endpoint |
| `STUFF_COUCH_DB` | `stuff` | Database name |
| `STUFF_COUCH_USER` | `stuff` | CouchDB user when using a password file |
| `STUFF_COUCH_PASSWORD_FILE` | empty | File containing the CouchDB password |
| `STUFF_TOKEN` | empty | API bearer token |
| `STUFF_TOKEN_FILE` | empty | File containing the API bearer token |

Credentials embedded in `STUFF_COUCH_URL` are supported for development. Password files are preferred for service deployments. CouchDB credentials are consumed only by the gateway and should not be distributed to CLI clients.

`GET /health` is an unauthenticated liveness endpoint. Other endpoints require the configured bearer token. If no token is configured, bind Stuff only to a trusted loopback or private interface.

## Scope boundaries

Stuff stores records about work. It does not perform work.

Stuff does **not** own:

- dispatch, execution, workers, retries, or recovery;
- locks, leases, schedules, or reconciliation;
- workflow states, notifications, or surfacing policy;
- project, epic, milestone, or dependency semantics;
- schema migrations or continuous validation.

Clients may represent any of those ideas in metadata and query them. That does not move their behavior into Stuff.

## Development

```bash
nix develop
go test ./...
go vet ./...
nix build
nix flake check
```

The test suite covers HTTP and CLI contracts, optimistic revision conflicts, point-in-time validation and drift, arbitrary JSON metadata, Mango passthrough, authentication, and query correction. Live integration is tested against CouchDB 3.x.
