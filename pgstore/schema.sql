-- chronicle's Postgres schema. Rendered by pgstore with $TABLE$ replaced by a
-- validated, quoted, optionally schema-qualified table name, and $NAME$ by the
-- bare table name used as a prefix for index and constraint names.
--
-- Every statement is idempotent, so Migrate can be run on every boot.

-- btree_gist supplies the equality operator classes the exclusion constraint
-- needs for the two text columns; GiST alone cannot combine "=" on text with
-- "&&" on a range. Creating it needs a role permitted to create extensions;
-- where that is not the deployment's shape, create it once out of band and the
-- statement becomes a no-op.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE IF NOT EXISTS $TABLE$ (
    -- The C collation is load-bearing. chronicle's total order breaks ties on
    -- the record ID with a byte-wise comparison, and a database whose default
    -- collation is anything else would order ties differently from the
    -- in-memory store -- which would not fail loudly, it would just hand out
    -- pages that skip and repeat rows relative to the reference implementation.
    id          text COLLATE "C" PRIMARY KEY,

    kind        text NOT NULL,
    entity_id   text NOT NULL,

    -- Opaque bytes, because Record.Data is opaque bytes under a pluggable
    -- Codec. A jsonb column here would quietly make JSON mandatory and turn a
    -- storage adapter into a codec mandate; the "query by changed field" path
    -- wants a projection alongside this, not a change of primary storage.
    data        bytea,

    -- NULL is unbounded on both axes, matching chronicle's zero time.Time.
    valid_from  timestamptz,
    valid_to    timestamptz,
    tx_from     timestamptz NOT NULL,
    tx_to       timestamptz,

    actor_id    text NOT NULL,
    actor_type  text NOT NULL DEFAULT '',
    actor_name  text NOT NULL DEFAULT '',
    reason      text NOT NULL DEFAULT '',
    intent      smallint NOT NULL DEFAULT 0,

    -- Always a string map, entirely under chronicle's control, so jsonb is
    -- safe here in a way it is not for data -- and it leaves a GIN index one
    -- statement away when filtering on metadata arrives.
    meta        jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Both axes as ranges, stored rather than computed per row, so that the
    -- GiST indexes and the exclusion constraint have something to point at.
    -- Half-open, matching chronicle's convention everywhere else.
    valid       tstzrange GENERATED ALWAYS AS (tstzrange(valid_from, valid_to, '[)')) STORED,
    tx          tstzrange GENERATED ALWAYS AS (tstzrange(tx_from, tx_to, '[)')) STORED,

    -- An empty range overlaps nothing, so a zero-width interval would slip
    -- past the exclusion constraint and sit in the log asserting nothing.
    CONSTRAINT $NAME$_valid_nonempty CHECK (NOT isempty(tstzrange(valid_from, valid_to, '[)'))),
    -- A closed transaction interval that does not advance is invisible to
    -- every as-of query, which is the quiet way a record disappears.
    CONSTRAINT $NAME$_tx_nonempty CHECK (tx_to IS NULL OR tx_to > tx_from),
    CONSTRAINT $NAME$_intent_known CHECK (intent BETWEEN 0 AND 2),
    CONSTRAINT $NAME$_actor_present CHECK (actor_id <> '')
);

-- The library's headline invariant, enforced by the database rather than
-- checked in Go: no two current records for one entity may cover the same
-- valid instant.
--
-- DEFERRABLE INITIALLY DEFERRED keeps the constraint correct under any
-- statement order. The shipped Apply closes superseded records before
-- inserting their replacements, so it never passes through an overlapping
-- state itself -- but the deferral costs nothing, and a future writer that
-- inserts first, or batches several writes into one transaction, stays
-- correct without anyone having to remember why.
DO $$
BEGIN
    ALTER TABLE $TABLE$ ADD CONSTRAINT $NAME$_no_overlap
        EXCLUDE USING gist (kind WITH =, entity_id WITH =, valid WITH &&)
        WHERE (tx_to IS NULL)
        DEFERRABLE INITIALLY DEFERRED;
EXCEPTION
    WHEN duplicate_table THEN NULL;
    WHEN duplicate_object THEN NULL;
END $$;

-- Ordering and keyset pagination. chronicle's total order is transaction
-- start, then valid start, then ID, and an unbounded valid start sorts first.
-- COALESCE to -infinity rather than an ORDER BY ... NULLS FIRST index, because
-- the keyset predicate is a row comparison and a row comparison against NULL
-- yields NULL rather than a boolean -- every resumed page would silently drop
-- the unbounded rows.
CREATE INDEX IF NOT EXISTS $NAME$_order
    ON $TABLE$ (tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id);

CREATE INDEX IF NOT EXISTS $NAME$_entity_order
    ON $TABLE$ (kind, entity_id, tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id);

CREATE INDEX IF NOT EXISTS $NAME$_actor_order
    ON $TABLE$ (actor_id, tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id);

-- Range containment and overlap on each axis. The exclusion constraint already
-- provides gist (kind, entity_id, valid) WHERE tx_to IS NULL, which is what
-- the log's overlap scan uses; these cover the same questions asked of history
-- rather than of current belief.
CREATE INDEX IF NOT EXISTS $NAME$_valid_gist
    ON $TABLE$ USING gist (kind, entity_id, valid);

CREATE INDEX IF NOT EXISTS $NAME$_tx_gist
    ON $TABLE$ USING gist (tx);

-- Legal holds. One row per hold, forever: placement writes the left half,
-- release fills the right half, and nothing deletes a row -- the lifecycle of
-- the control is itself an audit record. placed_at and released_at are
-- assigned by the store; effective_from is the operator's assertion of when
-- the preservation duty attached and may legitimately be backdated (FRCP
-- 37(e) triggers on anticipation, judged after the fact). NULL effective_from
-- means the duty has no asserted start.
CREATE TABLE IF NOT EXISTS $HOLDS$ (
    id               text PRIMARY KEY,
    kind             text NOT NULL DEFAULT '',
    entity_id        text NOT NULL DEFAULT '',
    effective_from   timestamptz,
    reason           text NOT NULL DEFAULT '',
    placed_by_id     text NOT NULL,
    placed_by_type   text NOT NULL DEFAULT '',
    placed_by_name   text NOT NULL DEFAULT '',
    placed_at        timestamptz NOT NULL,
    released_at      timestamptz,
    released_by_id   text NOT NULL DEFAULT '',
    released_by_type text NOT NULL DEFAULT '',
    released_by_name text NOT NULL DEFAULT '',
    release_reason   text NOT NULL DEFAULT '',

    CONSTRAINT $NAME$_h_placed CHECK (placed_by_id <> ''),
    -- A release without an actor is not a release chronicle performed.
    CONSTRAINT $NAME$_h_attributed CHECK (released_at IS NULL OR released_by_id <> '')
);

-- Tombstones: what remains of a chained record after retention destroyed it.
-- The chain hash is kept verbatim, format prefix included, so Verify can pass
-- over the gap; the coordinates are kept so the tombstone sorts where the
-- record used to. Deletion and tombstone insertion happen in one transaction,
-- and record_id as the key makes a retried deletion write the same tombstone
-- once.
CREATE TABLE IF NOT EXISTS $TOMBS$ (
    record_id  text COLLATE "C" PRIMARY KEY,
    kind       text NOT NULL,
    entity_id  text NOT NULL,
    valid_from timestamptz,
    tx_from    timestamptz NOT NULL,
    chain_hash text NOT NULL,
    deleted_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS $NAME$_t_entity
    ON $TOMBS$ (kind, entity_id, tx_from, COALESCE(valid_from, '-infinity'::timestamptz), record_id);
