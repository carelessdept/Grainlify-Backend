-- Structural guarantee that users.role cannot change on UPDATE without an
-- admin_role_audit row (#503).
--
-- Previously admin_role_audit recorded only what went through SetUserRole().
-- A direct `UPDATE users SET role = ...` - the documented break-glass recovery
-- path - changed the column and wrote nothing, making an empty audit table
-- ambiguous between "no role change ever occurred" and "a role change bypassed
-- the handler".
--
-- Backfill of role changes that predated this migration (2026-10-14) is not
-- possible: rows that were never written cannot be reconstructed, and a
-- synthetic row would be worse than the gap. From this migration onward, the
-- trigger is the single writer for role-update audit rows:
--
--   * SetUserRole sets transaction-local session variables
--     (`grainlify.role_audit_source`, `grainlify.role_audit_actor_user_id`,
--     `grainlify.role_audit_note`) in the same statement as its UPDATE and no
--     longer inserts into admin_role_audit itself (one writer, no double-write).
--   * Any direct UPDATE on users.role without those variables defaults to
--     `source = 'direct'` with a NULL `actor_user_id` and NULL `note`.

ALTER TABLE admin_role_audit
  DROP CONSTRAINT IF EXISTS admin_role_audit_source_check;

ALTER TABLE admin_role_audit
  ADD CONSTRAINT admin_role_audit_source_check
  CHECK (source IN ('admin_action', 'bootstrap', 'direct'));

CREATE OR REPLACE FUNCTION users_audit_role_change()
RETURNS TRIGGER AS $$
DECLARE
  v_source TEXT;
  v_actor_raw TEXT;
  v_actor UUID;
  v_note TEXT;
BEGIN
  v_source := COALESCE(NULLIF(current_setting('grainlify.role_audit_source', true), ''), 'direct');
  v_actor_raw := NULLIF(current_setting('grainlify.role_audit_actor_user_id', true), '');
  IF v_actor_raw IS NOT NULL THEN
    v_actor := v_actor_raw::uuid;
  END IF;
  v_note := NULLIF(current_setting('grainlify.role_audit_note', true), '');

  INSERT INTO admin_role_audit (subject_user_id, old_role, new_role, source, actor_user_id, note)
  VALUES (NEW.id, OLD.role, NEW.role, v_source, v_actor, v_note);

  -- Clear the transaction-local context after consuming it so a subsequent
  -- bare UPDATE in the same transaction does not inherit the previous caller's
  -- attribution.
  PERFORM set_config('grainlify.role_audit_source', '', true);
  PERFORM set_config('grainlify.role_audit_actor_user_id', '', true);
  PERFORM set_config('grainlify.role_audit_note', '', true);

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_users_audit_role_change ON users;
CREATE TRIGGER trg_users_audit_role_change
  AFTER UPDATE OF role ON users
  FOR EACH ROW
  WHEN (OLD.role IS DISTINCT FROM NEW.role)
  EXECUTE FUNCTION users_audit_role_change();
