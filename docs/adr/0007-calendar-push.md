# ADR 0007: Calendar push verifies and enqueues

## Status
Accepted (2026-10-25)

## Context
Clients learn about calendar changes only by polling `GET /v1/calendars/{id}/events`. Google watch channels and Microsoft Graph subscriptions can tell Relay that something changed. Graph requires a `2xx` within 3 seconds and will disable a subscription that is slow. Both subscriptions expire (Graph refuses to schedule `me/events` past 10,080 minutes). ADR 0003 already chose NATS JetStream for this kind of work. Delta sync, which applies the change, is separate.

## Decision
Connecting an account creates one provider subscription. The webhook does only this:

- compare the Google channel token or Graph `clientState` to a stored SHA-256, in constant time;
- publish `relay.sync.account` with the account id (`Nats-Msg-Id` dedupes a retry);
- respond. `sync` and unknown channels are `200` and queue nothing. A bad secret is `401`.

Lifecycle notices are queued too: renew, create again, or sync. The provider is not called inside the HTTP request. A background pass, same shape as the token refresher (`FOR UPDATE SKIP LOCKED`, one batch per tick), renews rows that expire within 24 hours. Google has no renew call, so that pass opens a new channel and then stops the old one. A provider `5xx` leaves the row for the next pass.

`WEBHOOK_BASE_URL` is optional. When it is unset, push stays off and reads stay on demand. A missing URL must not stop the API. `NATS_URL` is required only when push is on. Disconnect stops the subscription and deletes the row; the provider call is best effort and happens before the tokens are deleted.

## Consequences
- **Good:** A client does not have to poll to find out that a calendar changed. The request stays inside Graph's deadline. A duplicate notification does not become two jobs. Replicas share the renewal batch via `SKIP LOCKED` and do not use Redis as the safety lock.
- **Cost:** Push needs a public HTTPS URL. If the renewer is down, channels expire quietly; periodic delta sync is the backup and is not part of this change. The webhook has its own small per-address limit; a shared tenant limiter is still separate work.
