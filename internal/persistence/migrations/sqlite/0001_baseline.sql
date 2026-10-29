-- Fresh schema derived from the repository baseline; never replay legacy DROP migrations.
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT UNIQUE NOT NULL,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  avatar_url TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  is_verified INTEGER DEFAULT 0,
  verified_at TEXT,
  is_sponsor INTEGER DEFAULT 0,
  sponsor_since TEXT,
  last_login TEXT,
  last_ip TEXT,
  last_ip_location TEXT,
  last_ip_updated_at TEXT
, token_version INTEGER NOT NULL DEFAULT 0);

CREATE TABLE playlists (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  type TEXT NOT NULL CHECK (type IN ('local', 'synced')),
  is_default INTEGER NOT NULL DEFAULT 0,
  source TEXT,
  source_playlist_id TEXT,
  content_revision INTEGER NOT NULL DEFAULT 0,
  order_revision INTEGER NOT NULL DEFAULT 0,
  binding_version INTEGER NOT NULL DEFAULT 0,
  active_snapshot_id INTEGER,
  sync_status TEXT NOT NULL DEFAULT 'idle'
    CHECK (sync_status IN ('idle', 'syncing', 'ready', 'error')),
  last_synced_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')), sync_revision INTEGER NOT NULL DEFAULT 0,
  CHECK (
    (type = 'local' AND source IS NULL AND source_playlist_id IS NULL) OR
    (type = 'synced' AND source IS NOT NULL AND source_playlist_id IS NOT NULL)
  )
);

CREATE TABLE playlist_tracks_catalog (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  source TEXT NOT NULL,
  source_track_id TEXT NOT NULL,
  name TEXT NOT NULL,
  artists TEXT NOT NULL,
  album TEXT NOT NULL,
  pic_url TEXT NOT NULL DEFAULT '',
  source_ids_json TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(source, source_track_id)
);

CREATE TABLE playlist_snapshots (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  playlist_id INTEGER NOT NULL,
  binding_version INTEGER NOT NULL,
  expected_count INTEGER NOT NULL,
  item_count INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'staging'
    CHECK (status IN ('staging', 'complete', 'retired')),
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  completed_at TEXT,
  FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE
);

CREATE TABLE local_playlist_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  playlist_id INTEGER NOT NULL,
  track_id INTEGER NOT NULL,
  rank INTEGER NOT NULL,
  added_at TEXT NOT NULL DEFAULT (datetime('now')),
  FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE,
  FOREIGN KEY (track_id) REFERENCES playlist_tracks_catalog(id),
  UNIQUE(playlist_id, track_id)
);

CREATE TABLE playlist_snapshot_items (
  snapshot_id INTEGER NOT NULL,
  track_id INTEGER NOT NULL,
  source_position INTEGER NOT NULL,
  occurrence_index INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (snapshot_id) REFERENCES playlist_snapshots(id) ON DELETE CASCADE,
  FOREIGN KEY (track_id) REFERENCES playlist_tracks_catalog(id),
  PRIMARY KEY (snapshot_id, source_position, occurrence_index)
);

CREATE TABLE account_bindings (
  user_id INTEGER NOT NULL,
  provider TEXT NOT NULL,
  payload_json TEXT,
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(user_id, provider)
);

CREATE TABLE auth_rate_limits (
  key TEXT PRIMARY KEY,
  attempts INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE donations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  amount REAL,
  payment_type TEXT,
  status INTEGER DEFAULT 0,
  out_trade_no TEXT,
  trade_no TEXT,
  paid_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE favorites (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  track_id TEXT NOT NULL,
  name TEXT NOT NULL,
  artists TEXT NOT NULL,
  album TEXT NOT NULL,
  pic_url TEXT NOT NULL,
  source TEXT NOT NULL,
  added_at TEXT NOT NULL DEFAULT (datetime('now')), source_ids_json TEXT,
  UNIQUE(user_id, track_id, source)
);

CREATE TABLE listening_stats (
  user_id INTEGER PRIMARY KEY,
  total_listening_time REAL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE play_counts (
  user_id INTEGER NOT NULL,
  track_id TEXT NOT NULL,
  source TEXT NOT NULL,
  track_name TEXT,
  artists TEXT,
  album TEXT,
  pic_url TEXT,
  play_count INTEGER DEFAULT 0,
  first_played_at TEXT,
  last_played_at TEXT,
  UNIQUE(user_id, track_id, source)
);

CREATE TABLE playback_state (
  user_id INTEGER PRIMARY KEY,
  track_id TEXT,
  track_name TEXT,
  artists TEXT,
  album TEXT,
  pic_url TEXT,
  source TEXT,
  position INTEGER,
  is_from_playlist INTEGER,
  platform TEXT,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE playlist_operations (
  user_id INTEGER NOT NULL,
  operation_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
  response_json TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  expires_at TEXT NOT NULL,
  PRIMARY KEY (user_id, operation_id)
);

CREATE TABLE qr_login_sessions (
  rid TEXT PRIMARY KEY,
  code TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting', 'scanned', 'confirmed', 'expired', 'canceled')),
  desktop_device_name TEXT,
  desktop_ip TEXT,
  desktop_location TEXT,
  scanned_user_id INTEGER,
  confirmed_user_id INTEGER,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
, token_version INTEGER NOT NULL DEFAULT 0);

CREATE TABLE verification_codes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT NOT NULL,
  code TEXT NOT NULL,
  type TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used INTEGER DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_account_bindings_user_provider ON account_bindings(user_id, provider);

CREATE INDEX idx_auth_rate_limits_expiry ON auth_rate_limits(expires_at);

CREATE INDEX idx_donations_user_status ON donations(user_id, status);

CREATE INDEX idx_favorites_added_at ON favorites(added_at);

CREATE INDEX idx_favorites_user_id ON favorites(user_id);

CREATE INDEX idx_local_playlist_items_order
  ON local_playlist_items(playlist_id, rank ASC, id ASC);

CREATE INDEX idx_local_playlist_items_track
  ON local_playlist_items(track_id);

CREATE INDEX idx_play_counts_user_ranking
ON play_counts(user_id, play_count DESC, last_played_at DESC, track_id ASC, source ASC);

CREATE INDEX idx_play_counts_user_track_source ON play_counts(user_id, track_id, source);

CREATE INDEX idx_playlist_operations_expiry
  ON playlist_operations(expires_at);

CREATE INDEX idx_playlist_snapshot_items_order
  ON playlist_snapshot_items(snapshot_id, source_position ASC, occurrence_index ASC);

CREATE INDEX idx_playlist_snapshot_items_track
  ON playlist_snapshot_items(track_id);

CREATE INDEX idx_playlist_snapshots_playlist
  ON playlist_snapshots(playlist_id, id DESC);

CREATE UNIQUE INDEX idx_playlists_default_per_user
  ON playlists(user_id) WHERE is_default = 1;

CREATE UNIQUE INDEX idx_playlists_synced_source
  ON playlists(user_id, source, source_playlist_id) WHERE type = 'synced';

CREATE INDEX idx_playlists_user_created
  ON playlists(user_id, is_default DESC, created_at ASC);

CREATE INDEX idx_qr_login_sessions_confirmed_user_id ON qr_login_sessions(confirmed_user_id);

CREATE INDEX idx_qr_login_sessions_expires_at ON qr_login_sessions(expires_at);

CREATE INDEX idx_qr_login_sessions_scanned_user_id ON qr_login_sessions(scanned_user_id);

CREATE INDEX idx_qr_login_sessions_status ON qr_login_sessions(status);

CREATE INDEX idx_users_email ON users(email);

CREATE INDEX idx_users_username ON users(username);

CREATE INDEX idx_verification_codes_email ON verification_codes(email);

CREATE INDEX idx_verification_codes_expires ON verification_codes(expires_at);
