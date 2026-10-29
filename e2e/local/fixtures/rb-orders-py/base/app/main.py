"""Orders API.

Routes: POST /orders, GET /orders/{order_id}, DELETE /orders/{order_id},
POST /hooks/billing (webhook from rb-billing-php, event "invoice.paid"),
GET /events (SSE, event "order.updated"), GET /health.
"""

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, Field

from app.backend import UserNotFound, fetch_user
from app.events import Broker
from app.store import OrderStore

app = FastAPI(title="rb-orders-py")
store = OrderStore()
broker = Broker()


class Item(BaseModel):
    sku: str
    quantity: int = Field(gt=0)
    price: float = Field(ge=0)


class OrderIn(BaseModel):
    user_id: str
    items: list[Item]


@app.post("/orders", status_code=201)
def create_order(body: OrderIn) -> dict:
    try:
        user = fetch_user(body.user_id)
    except UserNotFound:
        raise HTTPException(status_code=422, detail="unknown user") from None
    total = round(sum(i.quantity * i.price for i in body.items), 2)
    order = store.add(body.user_id, user["email"], [i.model_dump() for i in body.items], total)
    broker.publish("order.updated", {"id": order.id, "status": order.status})
    return order.to_dict()


@app.get("/orders/{order_id}")
def get_order(order_id: int) -> dict:
    order = store.get(order_id)
    if order is None:
        raise HTTPException(status_code=404, detail="order not found")
    return order.to_dict()


@app.delete("/orders/{order_id}", status_code=204)
def delete_order(order_id: int) -> None:
    if not store.delete(order_id):
        raise HTTPException(status_code=404, detail="order not found")


@app.post("/hooks/billing")
async def billing_webhook(request: Request) -> dict:
    payload = await request.json()
    if payload.get("event") != "invoice.paid":
        return {"ignored": True}
    order = store.set_status(int(payload["order_id"]), "paid")
    if order is None:
        raise HTTPException(status_code=404, detail="order not found")
    broker.publish("order.updated", {"id": order.id, "status": order.status})
    return {"ok": True}


@app.get("/events")
async def events() -> StreamingResponse:
    return StreamingResponse(broker.stream(), media_type="text/event-stream")


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}
