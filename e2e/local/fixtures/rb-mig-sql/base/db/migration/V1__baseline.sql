CREATE TABLE customers (
    id    bigserial PRIMARY KEY,
    email text NOT NULL UNIQUE,
    name  text NOT NULL DEFAULT ''
);
