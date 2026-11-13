import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";

export default defineConfig({
  plugins: [svelte()],
  clearScreen: false,
  server: { port: 5183, strictPort: true },
  build: {
    target: "es2022",
    sourcemap: false,
    // The document viewer is a page of its own, sandboxed, and its libraries only load when a file is opened.
    rollupOptions: { input: { main: "index.html", viewer: "viewer.html" } },
  },
  // Classic workers: a sandboxed page can start those from its own code, but not module ones.
  worker: { format: "iife" },
});
