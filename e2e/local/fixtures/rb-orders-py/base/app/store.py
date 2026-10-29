"""In-memory order store (PostgreSQL in production)."""

import itertools
from dataclasses import asdict, dataclass, field


@dataclass
class Order:
    id: int
    user_id: str
    customer_email: str
    items: list[dict]
    total: float
    status: str = "new"
    history: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        data = asdict(self)
        data.pop("history")
        return data


class OrderStore:
    def __init__(self) -> None:
        self._orders: dict[int, Order] = {}
        self._ids = itertools.count(1)

    def add(self, user_id: str, email: str, items: list[dict], total: float) -> Order:
        order = Order(next(self._ids), user_id, email, items, total)
        order.history.append("created")
        self._orders[order.id] = order
        return order

    def get(self, order_id: int) -> Order | None:
        return self._orders.get(order_id)

    def delete(self, order_id: int) -> bool:
        return self._orders.pop(order_id, None) is not None

    def set_status(self, order_id: int, status: str) -> Order | None:
        order = self._orders.get(order_id)
        if order:
            order.status = status
            order.history.append(status)
        return order
