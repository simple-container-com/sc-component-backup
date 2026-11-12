import { defineConfig, type PluginOption } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";
import { mockApi } from "./mock/plugin";

export default defineConfig(({ mode }) => {
  const plugins: PluginOption[] = [react()];
  if (mode === "mock") plugins.push(mockApi());
  return {
    base: "/",
    plugins,
    resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
    server: {
      port: 5173,
      proxy:
        mode === "mock"
          ? undefined
          : { "/api": { target: "http://127.0.0.1:8080", changeOrigin: false } },
    },
    build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  };
});
