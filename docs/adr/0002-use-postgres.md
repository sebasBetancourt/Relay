# ADR 0002: Use PostgreSQL

## Status
Accepted (2026-10-18)

## Context
Relay stores tenants, connected accounts, encrypted OAuth tokens, and short-lived OAuth states. The data is relational (tenant → accounts → tokens), needs strong integrity (UNIQUE, FK), and critical writes such as Microsoft refresh-token rotation must be **all-or-nothing**.

## Options considered
- **PostgreSQL:** Relational model, ACID transactions, FK with `ON DELETE CASCADE`, native `enum`, `bytea`, `text[]`, and `timestamptz`.
- **MongoDB:** Flexible documents, but no real FK; our model is relational (tenants, accounts, 1:1 tokens, oauth_states), so integrity would live only in app code.

## Decision
We use **PostgreSQL** because the schema is relational, we need transactions for token updates, and Postgres gives us CASCADE FKs, enums, and the column types we already designed (`bytea` for ciphertext, `text[]` for scopes, `timestamptz` for expiry).

## Consequences
- **Good:** Transactional safety (e.g. persist new Microsoft refresh before using the new access token); DB enforces enums, UNIQUE, and CASCADE deletes; types match the design (`bytea`, `text[]`, `timestamptz`).
- **Cost:** Schema migrations and SQL discipline; less “schemaless” flexibility than a document store.
