// Live updates: SSE from rb-orders-py (through the gateway) and the
// notifications WebSocket of rb-backend.
import { API, BACKEND_WS_URL } from "../config";

export function subscribeOrders(onUpdate: (id: number, status: string) => void): EventSource {
  const source = new EventSource(`${API}/events`);
  source.addEventListener("order.updated", (e) => {
    const data = JSON.parse((e as MessageEvent).data) as { id: number; status: string };
    onUpdate(data.id, data.status);
  });
  return source;
}

export function connectNotifications(onUserCreated: (name: string) => void): WebSocket {
  const ws = new WebSocket(`${BACKEND_WS_URL}/ws/notifications`);
  ws.addEventListener("message", (e) => {
    const msg = JSON.parse(String(e.data)) as { event: string; data: { name: string } };
    if (msg.event === "user.created") {
      onUserCreated(msg.data.name);
    }
  });
  return ws;
}
