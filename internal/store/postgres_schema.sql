-- The store's one table, and the layers that keep it append-only
-- (docs/cloud-sql-design.md). Run by the owner, never by the server's user.
-- Every statement is safe to run again: nothing here drops or rewrites data.

CREATE TABLE IF NOT EXISTS doc (
  coll       text        NOT NULL,
  id         text        NOT NULL,
  fields     jsonb       NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (coll, id)
);

CREATE OR REPLACE FUNCTION doc_refuse_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'doc is append-only: % is refused', TG_OP
    USING ERRCODE = 'insufficient_privilege';
END
$$;

CREATE OR REPLACE TRIGGER doc_no_update_delete
  BEFORE UPDATE OR DELETE ON doc
  FOR EACH ROW EXECUTE FUNCTION doc_refuse_change();

CREATE OR REPLACE TRIGGER doc_no_truncate
  BEFORE TRUNCATE ON doc
  FOR EACH STATEMENT EXECUTE FUNCTION doc_refuse_change();
