// Runtime configuration injected by the hosting page (window.__CONFIG__).
export interface AppConfig {
  backendUrl: string;
}

declare global {
  interface Window {
    __CONFIG__?: Partial<AppConfig>;
  }
}

export const config: AppConfig = {
  backendUrl: window.__CONFIG__?.backendUrl ?? "http://localhost:8080",
};
