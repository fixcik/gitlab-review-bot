from decimal import Decimal

import pytest

from rb_py.pricing import Line, apply_promo, subtotal, total_with_vat


def test_subtotal():
    lines = [Line("a", 2, Decimal("1.25")), Line("b", 1, Decimal("10"))]
    assert subtotal(lines) == Decimal("12.50")


def test_subtotal_rejects_zero_quantity():
    with pytest.raises(ValueError):
        subtotal([Line("a", 0, Decimal("1"))])


def test_promo_known_and_unknown():
    assert apply_promo(Decimal("100"), "welcome10") == Decimal("90.00")
    assert apply_promo(Decimal("100"), "NOPE") == Decimal("100")
    assert apply_promo(Decimal("100"), None) == Decimal("100")


def test_total_with_vat():
    assert total_with_vat([Line("a", 1, Decimal("100"))], "SPRING5") == Decimal("114.00")
