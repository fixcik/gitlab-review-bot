-- +goose Up
ALTER TABLE orders DROP COLUMN note;

-- +goose Down
DROP TABLE orders;
