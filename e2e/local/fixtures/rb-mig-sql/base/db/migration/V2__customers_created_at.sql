ALTER TABLE customers ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
