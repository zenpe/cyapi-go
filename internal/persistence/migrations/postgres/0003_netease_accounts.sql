ALTER TABLE account_bindings ADD COLUMN revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE account_bindings ADD COLUMN status TEXT NOT NULL DEFAULT 'unbound';
ALTER TABLE account_bindings ADD COLUMN credentials TEXT NOT NULL DEFAULT '';
