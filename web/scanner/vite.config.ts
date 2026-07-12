/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Served through the gateway under /scanner/
export default defineConfig({
  base: '/scanner/',
  plugins: [react()],
  test: {
    environment: 'jsdom',
  },
})
