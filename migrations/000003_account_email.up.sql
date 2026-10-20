-- Informational email of the connected account (Google id_token email when
-- verified, Microsoft Graph mail/userPrincipalName). Nullable: older rows and
-- providers that return no email. Never a key: provider_account_id is.
ALTER TABLE connected_accounts ADD COLUMN email text;
