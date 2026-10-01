DROP TABLE IF EXISTS contact_group_members;
DROP TABLE IF EXISTS contact_groups;
ALTER TABLE contacts DROP COLUMN IF EXISTS enabled;
