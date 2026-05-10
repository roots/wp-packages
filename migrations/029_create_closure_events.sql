-- +goose Up
CREATE TABLE closure_events (
    id INTEGER PRIMARY KEY,
    vendor_name TEXT NOT NULL,
    vendor_slug TEXT NOT NULL,
    detected_at DATETIME NOT NULL,
    plugin_slugs TEXT NOT NULL, -- JSON array
    plugin_count INTEGER NOT NULL
);

CREATE INDEX idx_closure_events_vendor_slug ON closure_events(vendor_slug);
CREATE INDEX idx_closure_events_detected_at ON closure_events(detected_at);

-- +goose Down
DROP TABLE closure_events;
