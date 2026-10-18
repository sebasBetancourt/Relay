# ADR 0003: Use NATS JetStream

## Status
Accepted (2026-10-18)

## Context
In v0.2 Relay needs background work that must not run inside the HTTP request: provider webhooks (“an event changed”), first sync, and delta sync. Producers publish a message (“sync this account”); workers consume it later, with retries if a worker dies. That requires a message queue with durable storage.

## Options considered
- **NATS JetStream:** Lightweight message system with on-disk persistence; if a worker crashes, the message is kept and retried. Easy to run in a container.
- **Redis streams/lists:** Simpler to stand up, but weaker durability guarantees than JetStream for this use case.
- **Kafka:** Very capable, but heavy to operate for a small service.
- **AWS SQS:** Managed and durable, but locks the design to AWS.

## Decision
We use **NATS JetStream** for v0.2 async work: enqueue webhook and sync jobs, process them in workers with retries, without blocking the API request path.
Not used in v0.1; introduced in v0.2.

## Consequences
- **Good:** Durable queues in one small container; natural fit for webhooks, first sync, and delta sync with retries. Workers depend on a queue interface, so NATS can be swapped for SQS if we move to AWS after v1.0. Redis stays for locks and rate limiting, not for the queue.
- **Cost:** Extra infrastructure vs sync-only v0.1; need consumer/ack semantics and monitoring of pending messages.
