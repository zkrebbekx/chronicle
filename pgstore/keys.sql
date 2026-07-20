-- Per-subject data keys for chronicle's crypto-shredding. Rendered by pgstore
-- with $TABLE$ replaced by a validated, quoted, optionally schema-qualified
-- table name.
--
-- A destroyed subject keeps its row with the key nulled: the marker is what
-- makes destruction terminal, so no key can ever be minted again under an
-- identifier the caller believes erased. destroyed_at records the first
-- destruction and is never advanced by a repeat.
--
-- Read the caveat on pgstore.Keyring before relying on this table: keys
-- stored beside the data they protect are only as destroyed as every backup
-- of this table is.

CREATE TABLE IF NOT EXISTS $TABLE$ (
    subject      text PRIMARY KEY,
    key          bytea,
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    destroyed_at timestamptz,

    -- A NULL key is only legitimate as the residue of a destruction.
    CONSTRAINT $NAME$_key_or_destroyed CHECK (key IS NOT NULL OR destroyed_at IS NOT NULL)
);
