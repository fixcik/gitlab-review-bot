CREATE TABLE order_statuses (
    code  text PRIMARY KEY,
    title text NOT NULL
);

INSERT INTO order_statuses (code, title) VALUES
    ('new', 'New'),
    ('paid', 'Paid'),
    ('shipped', 'Shipped'),
    ('cancelled', 'Cancelled');
