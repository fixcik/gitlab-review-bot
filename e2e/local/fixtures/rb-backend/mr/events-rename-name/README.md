# rb-backend

Users service of the reviewbot sandbox.

- REST: `api/openapi.yaml` (`/api/v1/users…`), legacy accounts API `api/swagger.yaml`
  (Swagger 2.0), billing API `api/billing.yaml`.
- Events: `user.created` to the topic from `USERS_EVENTS_TOPIC` (`proto/users.proto`,
  JSON in `internal/events`), read by `rb-consumer`.
- WebSocket: `GET /ws/notifications`.
- Database: PostgreSQL, `migrations/`.

```sh
go test ./...
USERS_DB_DSN=postgres://... USERS_EVENTS_TOPIC=users.events go run ./cmd/server
```
