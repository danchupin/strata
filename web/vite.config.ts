import path from 'node:path';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Console is mounted at /console/ on the gateway port and embedded via go:embed.
// Keep base path aligned so asset URLs resolve under that prefix.
export default defineConfig({
  base: '/console/',
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    target: 'es2020',
  },
  server: {
    port: 5173,
  },
});
