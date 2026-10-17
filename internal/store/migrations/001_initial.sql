-- JunKloud initial schema. Applied by migrate.go inside one transaction.
--
-- Sessions, their frozen problems and learner marks are mutable only through
-- the Store's compare-and-swap Transition. Submissions, grading runs,
-- criterion results, validation records and review events are append-only:
-- the triggers below reject UPDATE and DELETE so a score can never be
-- overwritten, whichever code path (or manual SQL) tries it.

CREATE TABLE sessions (
    id                TEXT PRIMARY KEY,
    environment_id    TEXT NOT NULL,
    mode              TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN (
                          'PREPARING', 'RUNNING', 'GRADING', 'COMPLETED',
                          'SETUP_ERROR', 'GRADING_ERROR', 'ABANDONED')),
    parent_id         TEXT REFERENCES sessions(id),
    seed              INTEGER NOT NULL,          -- uint64 stored as its int64 bit pattern
    started_at        TEXT,                      -- UTC, NULL until the clock starts
    duration_limit_ns INTEGER NOT NULL DEFAULT 0,
    marks_json        TEXT
);
CREATE INDEX sessions_environment ON sessions (environment_id, state);

-- The per-session snapshot of each chosen problem (public content only; the
-- private package is keyed by hash elsewhere).
CREATE TABLE session_problems (
    session_id  TEXT NOT NULL REFERENCES sessions(id),
    problem_id  TEXT NOT NULL,
    ordinal     INTEGER NOT NULL,
    hash        TEXT NOT NULL,
    frozen_json TEXT NOT NULL,
    PRIMARY KEY (session_id, problem_id),
    UNIQUE (session_id, ordinal)
);

-- Intent records written before any external change, so an interrupted
-- operation can be found and recovered. No foreign keys on purpose: an
-- operation must be recordable even when its session row is missing.
CREATE TABLE operations (
    id             TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL,
    phase          TEXT NOT NULL,
    planned_json   TEXT NOT NULL,
    created_at     TEXT NOT NULL
);
CREATE INDEX operations_environment ON operations (environment_id, phase);

-- Everything JunKloud created, keyed by environment, session and problem
-- (empty strings for environment-level or session-level entries).
CREATE TABLE inventories (
    environment_id TEXT NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    problem_id     TEXT NOT NULL DEFAULT '',
    inventory_json TEXT NOT NULL,
    PRIMARY KEY (environment_id, session_id, problem_id)
);

CREATE TABLE submissions (
    id                   TEXT PRIMARY KEY,
    session_id           TEXT NOT NULL REFERENCES sessions(id),
    kind                 TEXT NOT NULL,
    parent_submission_id TEXT REFERENCES submissions(id),
    submitted_at         TEXT NOT NULL,
    input_hashes_json    TEXT
);
CREATE INDEX submissions_session ON submissions (session_id);

CREATE TABLE grading_runs (
    id            TEXT PRIMARY KEY,
    submission_id TEXT NOT NULL REFERENCES submissions(id),
    started_at    TEXT NOT NULL,
    finished_at   TEXT NOT NULL,
    score         INTEGER,                       -- NULL when the result is not determined
    max_score     INTEGER NOT NULL,
    complete      INTEGER NOT NULL CHECK (complete IN (0, 1))
);
CREATE INDEX grading_runs_submission ON grading_runs (submission_id);

CREATE TABLE criterion_results (
    run_id        TEXT NOT NULL REFERENCES grading_runs(id),
    ordinal       INTEGER NOT NULL,
    criterion_id  TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('PASS', 'FAIL', 'ERROR')),
    reason        TEXT NOT NULL DEFAULT '',
    points        INTEGER NOT NULL,
    evidence_json TEXT NOT NULL,
    PRIMARY KEY (run_id, criterion_id),
    UNIQUE (run_id, ordinal)
);

-- One row per validation; the newest row for a (problem hash, grader
-- version, environment hash) triple is the current certificate.
CREATE TABLE problem_validations (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    problem_hash     TEXT NOT NULL,
    grader_version   TEXT NOT NULL,
    environment_hash TEXT NOT NULL,
    seeds_json       TEXT NOT NULL,
    passed           INTEGER NOT NULL CHECK (passed IN (0, 1)),
    cases_json       TEXT NOT NULL
);
CREATE INDEX problem_validations_key
    ON problem_validations (problem_hash, grader_version, environment_hash, id);

CREATE TABLE review_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL,
    problem_id  TEXT NOT NULL,
    reviewed_at TEXT NOT NULL,
    FOREIGN KEY (session_id, problem_id)
        REFERENCES session_problems (session_id, problem_id)
);

CREATE TRIGGER submissions_no_update BEFORE UPDATE ON submissions
BEGIN SELECT RAISE(ABORT, 'submissions are append-only'); END;
CREATE TRIGGER submissions_no_delete BEFORE DELETE ON submissions
BEGIN SELECT RAISE(ABORT, 'submissions are append-only'); END;

CREATE TRIGGER grading_runs_no_update BEFORE UPDATE ON grading_runs
BEGIN SELECT RAISE(ABORT, 'grading runs are append-only'); END;
CREATE TRIGGER grading_runs_no_delete BEFORE DELETE ON grading_runs
BEGIN SELECT RAISE(ABORT, 'grading runs are append-only'); END;

CREATE TRIGGER criterion_results_no_update BEFORE UPDATE ON criterion_results
BEGIN SELECT RAISE(ABORT, 'criterion results are append-only'); END;
CREATE TRIGGER criterion_results_no_delete BEFORE DELETE ON criterion_results
BEGIN SELECT RAISE(ABORT, 'criterion results are append-only'); END;

CREATE TRIGGER problem_validations_no_update BEFORE UPDATE ON problem_validations
BEGIN SELECT RAISE(ABORT, 'validation records are append-only'); END;
CREATE TRIGGER problem_validations_no_delete BEFORE DELETE ON problem_validations
BEGIN SELECT RAISE(ABORT, 'validation records are append-only'); END;

CREATE TRIGGER review_events_no_update BEFORE UPDATE ON review_events
BEGIN SELECT RAISE(ABORT, 'review events are append-only'); END;
CREATE TRIGGER review_events_no_delete BEFORE DELETE ON review_events
BEGIN SELECT RAISE(ABORT, 'review events are append-only'); END;
