# ADR 0010: Event writes refuse what the providers would not store alike

## Status
Accepted (2026-10-30)

## Context
Relay was read-only. Writing through one API has three hard parts. Google takes RRULE and IANA zones; Graph takes `patternedRecurrence` and Windows zone names. The two also disagree on edge cases, such as a monthly event on the 31st. And a write is not safe to retry the way a read is: a client that repeats a request after a timeout must not create the meeting twice.

## Decision
- **One validated input.** `EventInput` and `EventPatch` become an `EventChange` before any provider is called, and each `Writer` only translates it. The two providers therefore accept and refuse exactly the same requests.
- **A common subset of recurrence.** `DAILY`, `WEEKLY` with `by_day`, `MONTHLY` by day of the month, and `YEARLY`, with `count`, `until` or no end. Anything outside it is `400 unsupported_recurrence`, never an approximation:
  - monthly on days 29 to 31 and yearly on February 29, because Google skips the short period and Outlook moves to its last day;
  - an event that starts outside its own `by_day`, because the providers treat the first occurrence differently.
- **The same week.** Graph gets `firstDayOfWeek: monday`, matching RRULE's default `WKST`, because with `interval > 1` the week start changes the dates.
- **Time zones from CLDR.** `gen_windowszones.go` now also writes IANA → Windows from every territory, and 001 wins a tie. A small alias table covers current names CLDR still lists under old ones (`Asia/Kolkata`). A zone that is still unknown is `400 unsupported_timezone`: two zones can share an offset today and differ at the next DST, so a nearby zone is not a safe guess.
- **Consent only when asked.** This takes the part of SCRUM-24 writes need. `GET /v1/connect/{provider}?features=calendar.write` adds `calendar.events` or `Calendars.ReadWrite`, and Google also gets `include_granted_scopes`. Stored scopes are checked before any call. A missing scope is `403 insufficient_scope` with `required_scopes` and `reconnect_url`, including when the provider itself refuses the token.
- **Idempotency in Postgres.** `Idempotency-Key` is claimed with one `INSERT … ON CONFLICT`, so concurrent replicas cannot both run. The hash covers the method, path and body. A different request with the same key is `422`, and one still running is `409`. Only successes are stored, for 24 hours; a failure releases the key. The janitor purges old rows.
- **Optimistic concurrency.** `Event.version` is the Google etag or the Graph `changeKey`. `If-Match` on PATCH passes through to the provider, and a mismatch is `412 version_conflict`.
- **Whole events only.** PATCH and DELETE read the event first and refuse a single occurrence. Moving a series needs its recurrence in the same request.
- **The same emails.** Google gets `sendUpdates=all`, because Graph always notifies attendees.

## Consequences
- **Good:** A client never learns provider differences by losing a meeting. Retries and concurrent edits cannot duplicate or overwrite events in normal operation.
- **Bad:** Valid patterns such as "the last Friday of the month" are refused until both translations exist.
- **Bad:** PATCH and DELETE cost one extra provider call.
- **Bad:** `GET /events` expands series, so a client only learns a series id from the create response. Editing one occurrence, or this and the following ones, is not supported.
- **Bad:** Idempotency is exact only around Relay. If a provider times out after creating the event, Relay answers `502` and releases the key, so a retry can duplicate it. If a process dies mid-request, its claim is taken over after a minute, with the same risk. Google's client-set event ids and Graph's `transactionId` could close this gap later.
- **Bad:** Stored responses hold event data in Postgres for 24 hours, in plain text, like `synced_events`.
- **Neutral:** `calendar.events` is a sensitive Google scope. Asking for it needs app verification before leaving Testing mode, and read-only tenants never see it.
- **Neutral:** Only the write part of SCRUM-24 exists here. The generic feature registry for other resources is still open.
