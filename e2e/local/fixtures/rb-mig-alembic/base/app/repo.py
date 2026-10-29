"""Queries of orders."""

from sqlalchemy import select
from sqlalchemy.orm import Session

from app.models import Order


def orders_by_author(session: Session, email: str) -> list[Order]:
    stmt = select(Order).where(Order.created_by == email).order_by(Order.id.desc())
    return list(session.scalars(stmt))


def create_order(session: Session, email: str, total: float) -> Order:
    order = Order(created_by=email, total=total)
    session.add(order)
    session.flush()
    return order
