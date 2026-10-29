"""SQLAlchemy models."""

from sqlalchemy import Column, DateTime, Integer, Numeric, String, Text, func
from sqlalchemy.orm import declarative_base

Base = declarative_base()


class Order(Base):
    __tablename__ = "orders"

    id = Column(Integer, primary_key=True)
    created_by = Column(String(180), nullable=False)
    status = Column(String(32), nullable=False, server_default="new")
    total = Column(Numeric(12, 2), nullable=False, server_default="0")
    note = Column(Text, nullable=True)
    created_at = Column(DateTime(timezone=True), server_default=func.now())
