// Service addresses: ORDERS_URL -> rb-orders-py, BACKEND_WS_URL -> rb-backend,
// API -> the public gateway (routes /events to rb-orders-py).
export const ORDERS_URL = process.env.ORDERS_URL ?? "http://localhost:8000";
export const API = process.env.API ?? "http://localhost:8000";
export const BACKEND_WS_URL = process.env.BACKEND_WS_URL ?? "ws://localhost:8080";
export const STATUS_URL = process.env.STATUS_URL ?? "http://localhost:8080";
