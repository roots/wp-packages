-- +goose Up
-- Widen the action CHECK constraint to allow 'tombstoned'. SQLite cannot ALTER
-- a CHECK constraint in place, so we rebuild the table.
CREATE TABLE status_check_changes_new (
    id INTEGER PRIMARY KEY,
    status_check_id INTEGER NOT NULL REFERENCES status_checks(id),
    package_type TEXT NOT NULL,
    package_name TEXT NOT NULL,
    action TEXT NOT NULL CHECK(action IN ('deactivated', 'reactivated', 'tombstoned')),
    created_at TEXT NOT NULL
);

INSERT INTO status_check_changes_new (id, status_check_id, package_type, package_name, action, created_at)
    SELECT id, status_check_id, package_type, package_name, action, created_at FROM status_check_changes;

DROP TABLE status_check_changes;
ALTER TABLE status_check_changes_new RENAME TO status_check_changes;

CREATE INDEX idx_status_check_changes_check_id ON status_check_changes(status_check_id);
CREATE INDEX idx_status_check_changes_created_at ON status_check_changes(created_at);

-- +goose Down
CREATE TABLE status_check_changes_new (
    id INTEGER PRIMARY KEY,
    status_check_id INTEGER NOT NULL REFERENCES status_checks(id),
    package_type TEXT NOT NULL,
    package_name TEXT NOT NULL,
    action TEXT NOT NULL CHECK(action IN ('deactivated', 'reactivated')),
    created_at TEXT NOT NULL
);

INSERT INTO status_check_changes_new (id, status_check_id, package_type, package_name, action, created_at)
    SELECT id, status_check_id, package_type, package_name, action, created_at
    FROM status_check_changes
    WHERE action IN ('deactivated', 'reactivated');

DROP TABLE status_check_changes;
ALTER TABLE status_check_changes_new RENAME TO status_check_changes;

CREATE INDEX idx_status_check_changes_check_id ON status_check_changes(status_check_id);
CREATE INDEX idx_status_check_changes_created_at ON status_check_changes(created_at);
