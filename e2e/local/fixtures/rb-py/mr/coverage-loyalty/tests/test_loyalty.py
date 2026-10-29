from decimal import Decimal

from rb_py.loyalty import loyalty_discount


def test_no_tier():
    assert loyalty_discount(Decimal("100"), None, 5) == Decimal("0.00")


def test_bronze():
    assert loyalty_discount(Decimal("100"), "bronze", 1) == Decimal("2.00")
