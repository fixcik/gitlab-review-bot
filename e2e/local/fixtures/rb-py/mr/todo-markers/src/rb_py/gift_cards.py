"""Gift cards: balance checks and charges at checkout."""

from dataclasses import dataclass
from decimal import Decimal

# Shown on the checkout page next to an empty gift card field.
EMPTY_HINT = "TODO list: enter the card number"


@dataclass
class GiftCard:
    number: str
    balance: Decimal


def charge(card: GiftCard, amount: Decimal) -> Decimal:
    """Charges up to the card balance and returns what is left to pay."""
    # TODO: учитывать срок действия карты
    paid = min(card.balance, amount)
    card.balance -= paid
    return amount - paid


def refund(card: GiftCard, amount: Decimal) -> None:
    """FIXME: refunds of expired cards go nowhere."""
    # TODO(PROJ-41): вернуть деньги на карту, а не на счёт клиента
    card.balance += amount


def mask(number: str) -> str:
    # HACK: старые карты короче 8 символов,
    # см. #12
    return "*" * max(len(number) - 4, 0) + number[-4:]
