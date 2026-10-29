CREATE TABLE daily_reports (
    day     date PRIMARY KEY,
    orders  integer NOT NULL DEFAULT 0,
    revenue numeric(12, 2) NOT NULL DEFAULT 0
);
