/// <reference types="vite/client" />
import type { MemcodeApi } from '../../preload'

declare global {
  interface Window {
    memcode: MemcodeApi
  }
}

export {}
