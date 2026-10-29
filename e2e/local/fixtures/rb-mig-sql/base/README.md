# rb-mig-sql

Go + PostgreSQL with three migration tools (on purpose, for the reviewbot
checks): goose `db/migrations/*.sql` (orders), Flyway `db/migration/V*__*.sql`
(customers, reference data), golang-migrate `migrations/*.up.sql`/`*.down.sql`.
