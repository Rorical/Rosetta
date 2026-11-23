/// <reference types="vitest" />
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    // The console's CSP forbids inline scripts, which the polyfill would add.
    modulePreload: { polyfill: false },
  },
  server: {
    proxy: { "/api": "http://127.0.0.1:8788" },
  },
});
