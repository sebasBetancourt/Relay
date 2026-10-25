# ADR 0008: Delta sync keeps a cursor and a copy

## Status
Accepted (2026-10-25)

## Context
A calendar webhook says that something changed on an account. It does not say what. Reading the whole calendar on every notification spends provider quota and still misses a notification that never arrived. Google Calendar returns changes since a `syncToken`. Microsoft Graph returns changes since a `deltaLink` on `calendarView/delta`. Both cursors expire (HTTP 410) and both are bound to the query that created them: a new time window is a different feed. ADR 0003 already put this work on NATS JetStream. ADR 0007 publishes `relay.sync.account` and does not apply it.

`GET /v1/calendars/{id}/events` is the public read. Serving it from a stored copy would be faster and would spend no quota, and it would also make the response as stale as the last sync. That is a contract change.

## Decision
Relay stores one cursor and one normalized copy of events per account.

- The first sync runs when the account connects. A worker publishes the same job for active accounts whose last sync is older than 15 minutes. The webhook job and that pass share one consumer.
- Google: `events.list` with `syncToken`, paging until `nextSyncToken`. An incremental call does not repeat the window. Microsoft: `GET /me/calendarView/delta`, following `@odata.nextLink` and `@odata.deltaLink` only when the host is the configured Graph host.
- Each change is `INSERT … ON CONFLICT (account_id, provider_event_id) DO UPDATE` and the update runs only when the version (Google etag, Graph `changeKey`) or the deleted flag differs. A removal sets `deleted`; the row stays.
- The new cursor is written in the same transaction as the rows. The transaction locks `account_sync_state` with `FOR UPDATE`. The provider call happens before that transaction, so the lock is not held across HTTP. If the stored cursor moved, the worker syncs once more from the new one. If the write fails, the old cursor remains.
- HTTP 410, or a window that no longer matches the configured 30 days, drops the cursor and lists the window from scratch.
- The events route is unchanged. It still calls the provider. The copy is the sync worker's state, not the API response.
- Disconnect deletes the copy and the cursor. Deleting the account row does the same through `ON DELETE CASCADE`. The cursor is not logged.
- `NATS_URL` empty disables the consumer and the periodic pass. The events route still works.

## Consequences
- **Good:** A notification becomes the changes since the last cursor, and applying the same change twice leaves the same rows. A crash does not skip a change or advance the cursor. A lost webhook is repaired by the next pass. Two replicas cannot commit the same account together.
- **Cost:** `synced_events` is a copy of the user's calendar and has to be deleted with the account. A cursor that expires goes quiet until the 410 response, which is why the full resync is automatic. The public read is still a live provider call; moving it onto the copy needs its own decision.
