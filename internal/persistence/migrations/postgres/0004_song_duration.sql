ALTER TABLE playlist_tracks_catalog ADD COLUMN duration_ms BIGINT CHECK (duration_ms IS NULL OR duration_ms BETWEEN 1 AND 86400000);
ALTER TABLE favorites ADD COLUMN duration_ms BIGINT CHECK (duration_ms IS NULL OR duration_ms BETWEEN 1 AND 86400000);
