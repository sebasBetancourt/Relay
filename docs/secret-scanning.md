# Secret scanning for Relay API keys

Relay API keys are designed so scanners can find them with low false positives:

| Part | Example | Role |
| --- | --- | --- |
| Brand + env | `rly_live_` / `rly_test_` | Identifiable prefix (like Stripe `sk_live_`) |
| Entropy | 43 base64url chars | 32 random bytes |
| Checksum | 6 base62 chars | CRC32 of the entropy (like GitHub `ghp_`) |

**Regex (Hyperscan / GitHub custom pattern):**

```text
rly_(live|test)_[A-Za-z0-9_-]{43}[0-9A-Za-z]{6}
```

## Register with GitHub

### A. Partner program (global public scanning)

To have GitHub scan **all** public repos for `rly_*` keys and notify Relay:

1. Read the [Secret scanning partner program](https://docs.github.com/en/code-security/secret-scanning/secret-scanning-partnership-program/secret-scanning-partner-program).
2. Email **secret-scanning@github.com** with:
   - Secret type name: `Relay API key`
   - Regex above
   - Sample valid / invalid strings (generate with `apikey.Generate`)
   - A public webhook that can receive match payloads and revoke keys
3. Implement signature verification and automatic revocation on the webhook.

Until that is approved, keys are **not** in GitHub’s default partner list.

### B. Custom pattern (your org / repo)

If you have GitHub Secret Protection on the org or repo:

1. **Settings → Advanced Security → Secret Protection → Custom patterns → New pattern**
2. Name: `Relay API key`
3. Secret format: the regex above
4. Dry run → Publish → optionally enable push protection

## Local tip

Never commit plaintext keys. `.env` is gitignored; use `KeyPrefix` (`rly_test_xxxxxxxx…`) in logs only.
