-- The environment version manifest a session was prepared against (spec 6.3):
-- the manifest itself, its hash and the grader version, frozen when the
-- session is created so later tasks can compare them with the live
-- environment. Existing rows have none (NULL). 001 and 002 stay unedited.

ALTER TABLE sessions ADD COLUMN grader_version            TEXT;
ALTER TABLE sessions ADD COLUMN environment_hash          TEXT;
ALTER TABLE sessions ADD COLUMN environment_manifest_json TEXT;

CREATE TRIGGER sessions_environment_frozen
BEFORE UPDATE OF grader_version, environment_hash, environment_manifest_json ON sessions
BEGIN SELECT RAISE(ABORT, 'a session''s environment manifest is frozen'); END;
