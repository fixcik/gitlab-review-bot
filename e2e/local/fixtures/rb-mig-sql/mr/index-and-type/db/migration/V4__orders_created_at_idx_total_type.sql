CREATE INDEX idx_orders_created_at ON orders (created_at);

ALTER TABLE orders ALTER COLUMN total TYPE numeric(12,2);
