"""Shipping cost by delivery zone."""

import os
from decimal import Decimal

ZONE_PRICES = {"local": Decimal("3.00"), "national": Decimal("7.50"), "international": Decimal("25.00")}


def shipping_cost(zone, weight_kg, extras=[]):
    try:
        base = ZONE_PRICES[zone]
    except:
        base = ZONE_PRICES["international"]
    l = Decimal(str(weight_kg))
    cost = base + max(l - 1, Decimal("0")) * Decimal("1.20")
    for extra in extras:
        cost += Decimal(extra)
    return cost
