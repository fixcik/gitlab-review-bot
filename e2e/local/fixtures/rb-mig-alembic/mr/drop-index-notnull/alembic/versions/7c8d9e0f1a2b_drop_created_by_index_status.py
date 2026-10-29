"""drop created_by, index status, note not null

Revision ID: 7c8d9e0f1a2b
Revises: 1a2b3c4d5e6f
Create Date: 2025-10-20 12:00:00
"""

from alembic import op

revision = "7c8d9e0f1a2b"
down_revision = "1a2b3c4d5e6f"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.drop_column("orders", "created_by")
    op.create_index("ix_orders_status", "orders", ["status"])
    op.alter_column("orders", "note", nullable=False)


def downgrade() -> None:
    op.drop_table("orders")
