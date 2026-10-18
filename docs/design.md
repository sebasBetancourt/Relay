# Relay v0.1
## Microsoft - Google - Calendars API

### Problem
- Many enterprises and startups, integrate microsoft and Google, all yours tools, whats they a used for the same, a developer, would have integrate one by one.
- Emerge problems with the rotation of tokens, clients, principally for a API to robust with it is Microsoft, and development from zero whats i will take weeks, and depends of the requerid tools or apps of this services.
-  I offer a solution, the unificated of the two APIs. For the version 1.0, we will take the apps of calendar of google and outlook, in a one API aplicated the due security and a integration secure and scalable.

### Scopes

#### Scope v0.1
- Management minim of users, tenats with API key
- OAuth 2.0 authorization code flow + PKCE with Google and Microsoft
- Library of Microsft Integrate MSAL
- Google, directlly OAuth 2.0 with library of Golang golang.org/x/oauth2
- Minimun Scopes incrementals, client secret and .envs
- Vault of tokens, cifrate and correct refresh - Microsoft save the refresh token every time
- List events with the same format for the two suppliers - GET api/v1/calendar/{id}/events
- Enviroments Docker and Postgresql
- Documentation, Readme with diagram and how starts the proyect.
- Cifrate of tokens in the Database AES - GCM
- Validate State in the callback, avoid attacks CSRF
- Disconnect a account and revoque their tokens
- Manage tokens revoques
- Test and CI github actions

#### Scope v02.0
- Workers for the first sincronization and the reconection
- Webhooks of calendars - Google watchs channels and subscriptions graphs 
- Delta Sync with backup, if to lost a webhook
- Redis, Throlling, and rate limiting for tenat

#### Scope v03.0
- Landing Page with documentation OpenAPI

### Diagram
```mermaid
flowchart LR
    Client["Client / App"] -->|"HTTP"| API["Relay API in Go"]
    API --> Postgres[(Postgres)]
    Google["Google Calendar API"]
    Microsoft["Microsoft Graph API"]
```

### Diagram OAuth and events
```mermaid
sequenceDiagram
    participant Client as Cliente App
    participant API as Relay API
    participant DB as Postgres
    participant Provider as Google Microsoft

    Client->>API: 1. GET /v1/connect/{provider}
    API->>DB: 1b. Save state and PKCE
    API-->>Client: 2. Redirect to OAuth
    Client->>Provider: Consent screen
    Provider->>API: 3. Callback with code and state
    API->>DB: 3b. Validate state and get PKCE verifier
    API->>Provider: 4. Exchange code for tokens
    Provider-->>API: 5. access_token and refresh_token
    API->>DB: 6. Save encrypted tokens

    Client->>API: 7. GET /v1/calendars/:id/events
    API->>DB: 8. Get active token
    DB-->>API: 9. Return tokens
    opt If access_token expired
        API->>Provider: 10a. Refresh token
        Provider-->>API: 10b. New tokens
        Note right of Provider: Microsoft rotates the refresh token on every refresh
        API->>DB: 10c. Save updated tokens
    end
    API->>Provider: 11. Ask events with access_token
    Provider-->>API: 12. Raw events
    API-->>Client: 13. Normalized events
```
