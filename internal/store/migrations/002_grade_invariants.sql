-- A complete grading run has a score and no ERROR result; an incomplete one
-- has no score. Store.SaveRun checks this first (and also requires at least
-- one result); these triggers are the database-level backstop. Triggers are
-- used because SQLite cannot add a CHECK to an existing table, and 001 stays
-- unedited.

CREATE TRIGGER grading_runs_grade_invariant BEFORE INSERT ON grading_runs
WHEN (NEW.complete = 1 AND NEW.score IS NULL) OR (NEW.complete = 0 AND NEW.score IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'a complete run needs a score and an incomplete run must not have one'); END;

CREATE TRIGGER criterion_results_no_error_in_complete_run BEFORE INSERT ON criterion_results
WHEN NEW.status = 'ERROR' AND (SELECT complete FROM grading_runs WHERE id = NEW.run_id) = 1
BEGIN SELECT RAISE(ABORT, 'a complete run cannot contain an ERROR result'); END;
