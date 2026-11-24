import { defineConfig } from "@playwright/test"

export default defineConfig({
  testDir: "./tests",
  timeout: 45000,
  use: { channel: process.env.PLAYWRIGHT_CHANNEL || undefined },
})
