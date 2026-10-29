// Build-time environment injected by the bundler (process.env.*).
declare const process: {
  env: {
    ORDERS_URL?: string;
    API?: string;
    BACKEND_WS_URL?: string;
    STATUS_URL?: string;
  };
};
