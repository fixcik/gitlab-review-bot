-- +goose Up
CREATE TABLE orders (
    id          bigserial PRIMARY KEY,
    customer_id bigint NOT NULL,
    created_by  text NOT NULL,
    status      text NOT NULL DEFAULT 'new',
    total       numeric(10, 2) NOT NULL DEFAULT 0,
    note        text,
    legacy_ref  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE orders;
