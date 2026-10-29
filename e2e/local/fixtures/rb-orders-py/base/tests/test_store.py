from app.store import OrderStore


def test_add_and_status():
    s = OrderStore()
    o = s.add("u1", "a@example.com", [{"sku": "a", "quantity": 1, "price": 5.0}], 5.0)
    assert s.get(o.id).status == "new"
    s.set_status(o.id, "paid")
    assert s.get(o.id).history == ["created", "paid"]
