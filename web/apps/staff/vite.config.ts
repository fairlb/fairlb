import tailwindcss from "@tailwindcss/vite";
import { brandBuild } from "@fairlb/brand/build";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const brand = brandBuild("communityAdmin");

export default defineConfig({
  define: brand.define,
  plugins: [brand.plugin, react(), tailwindcss()],
  build: {
    // Code splitting is on so that a heavy dependency used by one page — the
    // charting library, for instance — lands in its own chunk instead of the
    // entry. The build is embedded into a single binary either way, so having
    // several chunks costs nothing at distribution time.
    rolldownOptions: { output: { codeSplitting: true } },
  },
  server: {
    port: 5175,
    // Dev backend: `go run ./cmd/fairlb serve`, which serves every plane from
    // one host and separates them by path.
    proxy: { "/api": "http://localhost:8080" },
  },
});
