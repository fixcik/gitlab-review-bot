DO $$
BEGIN
    EXECUTE 'ALTER TABLE orders DROP COLUMN created_by';
END $$;
