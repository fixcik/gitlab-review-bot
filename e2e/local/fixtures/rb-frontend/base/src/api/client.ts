// Client of the rb-backend users API (api/openapi.yaml in rb-backend).
import { config } from "../config";

export interface User {
  id: string;
  email: string;
  name: string;
  created_at: string;
}

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${config.backendUrl}${path}`, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });
  if (!res.ok) {
    throw new ApiError(res.status, `${init?.method ?? "GET"} ${path}: ${res.status}`);
  }
  return (await res.json()) as T;
}

export function getUser(id: string): Promise<User> {
  return request<User>(`/api/v1/users/${encodeURIComponent(id)}`);
}

export function listUsers(limit = 50): Promise<User[]> {
  return request<User[]>(`/api/v1/users?limit=${limit}`);
}

export function createUser(email: string, name: string): Promise<User> {
  return request<User>("/api/v1/users", {
    method: "POST",
    body: JSON.stringify({ email, name }),
  });
}
