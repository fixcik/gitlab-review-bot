import { createOrder, getOrder, type Order } from "../api/orders";

export function formatTotal(order: Order): string {
  return `${order.total.toFixed(2)} ₽`;
}

export async function showOrder(root: HTMLElement, id: number): Promise<void> {
  const order = await getOrder(id);
  root.textContent = `#${order.id} — ${order.status} — ${formatTotal(order)}`;
}

export async function quickOrder(root: HTMLElement, userId: string, sku: string): Promise<void> {
  const order = await createOrder(userId, [{ sku, quantity: 1, price: 0 }]);
  root.textContent = `Создан заказ #${order.id} на ${formatTotal(order)}`;
}
