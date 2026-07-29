import { vitePlugin as remix } from "@remix-run/dev";
import { defineConfig } from "vite";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath } from "url";
import path from "path";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  build: {
    rollupOptions: {
      output: {
        manualChunks(id) {
          const normalized = id.split(path.sep).join("/");
          if (!normalized.includes("node_modules")) return undefined;
          // Keep shared framework code and lucide icons in stable vendor chunks
          // so route navigation does not fan out into many tiny module requests.
          if (normalized.includes("node_modules/lucide-react")) {
            return "vendor-icons";
          }
          if (
            normalized.includes("node_modules/@remix-run") ||
            normalized.includes("node_modules/react/") ||
            normalized.includes("node_modules/react-dom/") ||
            normalized.includes("node_modules/.vite/deps/@remix-run") ||
            normalized.includes("node_modules/.vite/deps/react")
          ) {
            return "vendor-react";
          }
          return undefined;
        },
      },
    },
  },
  resolve: {
    alias: {
      "~": path.resolve(__dirname, "app"),
    },
  },
  plugins: [
    tailwindcss(),
    remix({
      ssr: false,
      future: {
        v3_fetcherPersist: true,
        v3_relativeSplatPath: true,
        v3_throwAbortReason: true,
      },
    }),
  ],
});
