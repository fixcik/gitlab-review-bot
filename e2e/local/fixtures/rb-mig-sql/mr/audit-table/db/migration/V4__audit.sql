CREATE TABLE audit (
    id         bigserial PRIMARY KEY,
    order_id   bigint NOT NULL,
    action     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit ON audit (id);

ALTER TABLE audit ADD CONSTRAINT audit_order_fk FOREIGN KEY (order_id) REFERENCES orders (id);

CREATE INDEX CONCURRENTLY idx_orders_status ON orders (status);

CREATE INDEX idx_orders_customer ON orders (customer_id);
