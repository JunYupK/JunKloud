-- What a submission covers and how it was timed (spec 9.1, 9.2): the problem
-- (NULL for a whole-session submission), the solving time up to the
-- submission with the overtime, the within-limit flag and the clock anomaly
-- flag, and the problems whose solution the learner had already opened.
-- Existing rows read as whole-session submissions without timing. 001-003
-- stay unedited; the append-only triggers only reject UPDATE and DELETE, so
-- they are unaffected by new columns.

ALTER TABLE submissions ADD COLUMN problem_id            TEXT;
ALTER TABLE submissions ADD COLUMN solve_duration_ns     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE submissions ADD COLUMN overtime_ns           INTEGER NOT NULL DEFAULT 0;
ALTER TABLE submissions ADD COLUMN within_limit          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE submissions ADD COLUMN clock_anomaly         INTEGER NOT NULL DEFAULT 0;
ALTER TABLE submissions ADD COLUMN viewed_solutions_json TEXT;
