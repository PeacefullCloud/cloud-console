-- Who may get an account created automatically when they sign in through a
-- provider: 'open' (anyone the provider authenticates), 'domains' (verified
-- email in allowed_domains) or 'link_only' (never; accounts must exist and be
-- linked from Settings).
--
-- Existing providers keep 'open' so upgrading does not change who can sign in.
ALTER TABLE sso_providers ADD COLUMN provisioning TEXT NOT NULL DEFAULT 'open';
ALTER TABLE sso_providers ADD COLUMN allowed_domains TEXT NOT NULL DEFAULT '';
