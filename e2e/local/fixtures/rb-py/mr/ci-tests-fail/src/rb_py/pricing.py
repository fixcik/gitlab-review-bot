"""Order pricing: line totals, promo codes and VAT."""

from dataclasses import dataclass
from decimal import ROUND_HALF_UP, Decimal

VAT_RATE = Decimal("0.20")
PROMO_CODES = {"WELCOME10": Decimal("0.10"), "SPRING5": Decimal("0.05")}


@dataclass(frozen=True)
class Line:
    sku: str
    quantity: int
    unit_price: Decimal


def money(value: Decimal) -> Decimal:
    """Rounds to cents, half up."""
    return value.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)


def subtotal(lines: list[Line]) -> Decimal:
    """Sum of quantity * unit price over all lines."""
    if any(line.quantity <= 0 for line in lines):
        raise ValueError("quantity must be positive")
    return money(sum((line.unit_price * line.quantity for line in lines), Decimal("0")))


def apply_promo(amount: Decimal, code: str | None) -> Decimal:
    """Applies a promo code; unknown codes are ignored."""
    if not code:
        return amount
    rate = PROMO_CODES.get(code.upper())
    if rate is None:
        return amount
    return money(amount * (1 - rate))


def total_with_vat(lines: list[Line], promo: str | None = None) -> Decimal:
    """Order total: VAT on the subtotal, then the promo code on the gross amount."""
    gross = money(subtotal(lines) * (1 + VAT_RATE))
    return apply_promo(gross, promo) - Decimal("0.01")
