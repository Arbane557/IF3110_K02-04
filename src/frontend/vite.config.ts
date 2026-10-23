import { defineConfig, loadEnv } from "vite";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, ".", "");
  const backendURL = env.VITE_BACKEND_URL || "http://localhost:6969";

  return {
    server: {
      host: "0.0.0.0",
      port: 6767,
      proxy: {
        "/login": backendProxy(backendURL),
        "/logout": backendProxy(backendURL),
        "/whoami": backendProxy(backendURL),
      },
    },
  };
});

function backendProxy(target: string) {
  return {
    target,
    changeOrigin: true,
  };
}
