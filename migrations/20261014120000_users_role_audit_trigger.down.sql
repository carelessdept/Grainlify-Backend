DROP TRIGGER IF EXISTS trg_users_audit_role_change ON users;
DROP FUNCTION IF EXISTS users_audit_role_change();

DELETE FROM admin_role_audit WHERE source = 'direct';

ALTER TABLE admin_role_audit
  DROP CONSTRAINT IF EXISTS admin_role_audit_source_check;

ALTER TABLE admin_role_audit
  ADD CONSTRAINT admin_role_audit_source_check
  CHECK (source IN ('admin_action', 'bootstrap'));
