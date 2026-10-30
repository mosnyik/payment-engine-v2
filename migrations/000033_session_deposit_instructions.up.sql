-- Snapshot the exact instructions returned by the collection provider.
ALTER TABLE sessions ADD COLUMN deposit_instructions JSONB;
