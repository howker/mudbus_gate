@'
CREATE TABLE IF NOT EXISTS readings_current (
    device_id TEXT NOT NULL,
    point_id TEXT NOT NULL,
    instance TEXT NOT NULL DEFAULT '',
    value_text TEXT,
    unit TEXT,
    quality TEXT,
    quality_reason TEXT,
    ts DATETIME NOT NULL,
    PRIMARY KEY (device_id, point_id, instance)
);

CREATE TABLE IF NOT EXISTS readings_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL,
    point_id TEXT NOT NULL,
    instance TEXT NOT NULL DEFAULT '',
    value_text TEXT,
    unit TEXT,
    quality TEXT,
    quality_reason TEXT,
    ts DATETIME NOT NULL
);
'@ | Set-Content internal\storage\sqlite\migrations.sql -Encoding UTF8