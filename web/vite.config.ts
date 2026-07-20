import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The SPA is served from harmostes-ui at /static/spa/.
// All asset URLs are prefixed with this base so the Go file server
// (http.FileServer at /static/) serves them correctly.
export default defineConfig({
  plugins: [react()],
  base: "/static/spa/",
  server: {
    proxy: {
      "/api": "http://localhost:8083",
    },
  },
  build: {
    outDir: "./dist",
    emptyOutDir: true,
  },
});
