import { createContext } from 'react'
import { submissionKey } from './operations'

export type SubmitResult = {
  value?: unknown
  block?: string
}

export type SubmitHook = () => Promise<SubmitResult>

type Pressed = { name: string; context?: Record<string, unknown> } & Record<string, unknown>

export class SubmitCaptures {
  private readonly hooks = new Map<string, SubmitHook>()
  private readonly flying = new Map<string, Promise<void>>()
  private readonly listeners = new Set<() => void>()

  register(field: string, hook: SubmitHook): () => void {
    this.hooks.set(field, hook)
    return () => {
      if (this.hooks.get(field) === hook) this.hooks.delete(field)
    }
  }

  key(action: Pressed): string {
    const context = { ...(action.context ?? {}) }
    for (const field of this.hooks.keys()) delete context[field]
    const { timestamp: _timestamp, ...rest } = action
    return submissionKey('captures', { ...rest, context })
  }

  get busy(): boolean {
    return this.flying.size > 0
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener)
    return () => void this.listeners.delete(listener)
  }

  private notify() {
    for (const listener of this.listeners) listener()
  }

  once(action: Pressed, run: (key: string) => Promise<void>): Promise<void> {
    const key = this.key(action)
    const going = this.flying.get(key)
    if (going) return going
    const promise = run(key).finally(() => {
      this.flying.delete(key)
      this.notify()
    })
    this.flying.set(key, promise)
    this.notify()
    return promise
  }

  async apply(action: Pressed): Promise<{ context?: Record<string, unknown>; block?: string }> {
    const context = action.context
    if (!context || this.hooks.size === 0) return { context }
    const next = { ...context }
    for (const [field, hook] of this.hooks) {
      if (!(field in context)) continue
      const result = await hook()
      if (result.block) return { block: result.block }
      if (result.value !== undefined) next[field] = result.value
    }
    return { context: next }
  }
}

export const SubmitCapturesContext = createContext<SubmitCaptures | null>(null)
