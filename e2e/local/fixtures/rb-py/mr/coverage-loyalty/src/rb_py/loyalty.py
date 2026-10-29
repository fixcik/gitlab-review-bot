"""Loyalty discounts."""

from decimal import Decimal

from rb_py.pricing import money

TIERS = {"bronze": Decimal("0.02"), "silver": Decimal("0.05"), "gold": Decimal("0.10")}
MAX_DISCOUNT = Decimal("50.00")


def loyalty_discount(amount: Decimal, tier: str | None, orders_count: int) -> Decimal:
    """Discount for a returning customer, capped at MAX_DISCOUNT."""
    if tier is None or orders_count < 1:
        return Decimal("0.00")
    rate = TIERS.get(tier.lower())
    if rate is None:
        raise ValueError(f"unknown loyalty tier: {tier}")
    if orders_count >= 50:
        rate += Decimal("0.03")
    elif orders_count >= 10:
        rate += Decimal("0.01")
    discount = money(amount * rate)
    if discount > MAX_DISCOUNT:
        return MAX_DISCOUNT
    return discount


def price_after_loyalty(amount: Decimal, tier: str | None, orders_count: int) -> Decimal:
    """Amount minus the loyalty discount, never below zero."""
    return max(money(amount - loyalty_discount(amount, tier, orders_count)), Decimal("0.00"))
