import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// `npm run dev` proxies the API to a local server (MEMSPACE_DEV_SERVER).
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true, sourcemap: false },
  server: {
    proxy: { '/api': process.env.MEMSPACE_DEV_SERVER ?? 'http://127.0.0.1:8765' },
  },
})
