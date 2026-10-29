// Order history from rb-orders-py.
import { ORDERS_URL } from "../config";

export interface OrderHistory {
  id: number;
  history: string[];
}

export async function getOrderHistory(id: number): Promise<OrderHistory> {
  const res = await fetch(`${ORDERS_URL}/orders/${id}/history`);
  if (!res.ok) {
    throw new Error(`order history: ${res.status}`);
  }
  return (await res.json()) as OrderHistory;
}

const REPORTS_URL = "https://reports.internal.example";

export async function dailyReport(): Promise<unknown> {
  const res = await fetch(`${REPORTS_URL}/reports/daily`);
  return res.json();
}

export async function latestRelease(): Promise<string> {
  const res = await fetch("https://api.github.com/repos/fixcik/gitlab-review-bot/releases/latest");
  const body = (await res.json()) as { tag_name: string };
  return body.tag_name;
}
