--liquibase formatted sql

-- One person can appear as several Azure DevOps identities (account re-creation, guest
-- accounts, UPN conflicts). A merge folds a secondary identity into a canonical one for all
-- reporting. Resolution happens at query time, so adding or removing a merge re-credits
-- history immediately. Merges are maintained by people, never by the collector.
-- Git emails are attached to identities separately through identity_aliases (any number of
-- emails per identity).

--changeset dashboard:002b-identity-merges
CREATE TABLE identity_merges (
    identity_id text PRIMARY KEY REFERENCES identities (identity_id) ON DELETE CASCADE,
    canonical_identity_id text NOT NULL REFERENCES identities (identity_id) ON DELETE CASCADE,
    note text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (identity_id <> canonical_identity_id)
);
CREATE INDEX identity_merges_canonical_idx ON identity_merges (canonical_identity_id);

--changeset dashboard:002b-identity-merges-single-level splitStatements:false
-- Keep merges one level deep: a canonical identity cannot itself be merged, and an identity
-- that other identities merge into cannot be merged away.
CREATE FUNCTION identity_merges_single_level() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM identity_merges WHERE identity_id = NEW.canonical_identity_id) THEN
        RAISE EXCEPTION 'canonical identity % is itself merged; merge into its canonical identity instead', NEW.canonical_identity_id;
    END IF;
    IF EXISTS (SELECT 1 FROM identity_merges WHERE canonical_identity_id = NEW.identity_id) THEN
        RAISE EXCEPTION 'identity % is the canonical identity of other merges; re-point those first', NEW.identity_id;
    END IF;
    RETURN NEW;
END
$$;

--changeset dashboard:002b-identity-merges-trigger
CREATE TRIGGER identity_merges_single_level BEFORE INSERT OR UPDATE ON identity_merges
    FOR EACH ROW EXECUTE FUNCTION identity_merges_single_level();
GRANT SELECT ON identity_merges TO dashboard_read;
