// Debug helper; the query is shown on /map as text (U10: HTML must be escaped).
import { ORDERS_URL } from "./config";

export function debugSearch(): Promise<Response> {
  return fetch(`${ORDERS_URL}/orders/search?q=<img src=x onerror=alert(1)>`);
}
