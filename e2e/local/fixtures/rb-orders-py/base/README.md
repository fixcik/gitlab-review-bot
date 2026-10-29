# rb-orders-py

Orders service (FastAPI). Calls rb-backend `GET /api/v1/users/{id}`
(`BACKEND_URL`), receives `invoice.paid` from rb-billing-php on
`POST /hooks/billing`, streams `order.updated` on `GET /events` (SSE).
