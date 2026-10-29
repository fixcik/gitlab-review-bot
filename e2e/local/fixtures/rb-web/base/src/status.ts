// Status page: /health is served by both rb-backend and rb-orders-py, the
// base decides which one answers.
import { STATUS_URL } from "./config";

export async function isHealthy(): Promise<boolean> {
  const res = await fetch(`${STATUS_URL}/health`);
  return res.ok;
}
