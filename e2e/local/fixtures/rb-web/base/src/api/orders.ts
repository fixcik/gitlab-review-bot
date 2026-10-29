// Calls of rb-orders-py.
import axios from "axios";
import { ORDERS_URL } from "../config";

export interface OrderItem {
  sku: string;
  quantity: number;
  price: number;
}

export interface Order {
  id: number;
  user_id: string;
  status: string;
  total: number;
  items: OrderItem[];
}

export const ordersApi = axios.create({ baseURL: process.env.ORDERS_URL, timeout: 5000 });

export async function createOrder(userId: string, items: OrderItem[]): Promise<Order> {
  const res = await fetch(`${ORDERS_URL}/orders`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ user_id: userId, items }),
  });
  if (!res.ok) {
    throw new Error(`create order: ${res.status}`);
  }
  return (await res.json()) as Order;
}

export async function getOrder(id: number): Promise<Order> {
  const res = await ordersApi.get<Order>(`/orders/${id}`);
  return res.data;
}

export async function cancelOrder(id: number): Promise<void> {
  await ordersApi.delete(`/orders/${id}`);
}

// Export endpoint planned in rb-orders-py, not implemented there yet.
export async function exportOrders(): Promise<Blob> {
  const res = await fetch(`${ORDERS_URL}/orders/export?format=csv`);
  return res.blob();
}
