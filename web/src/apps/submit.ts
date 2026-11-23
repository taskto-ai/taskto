// What a form does as it is submitted, before its action is sent: a location field whose
// capture is "submit" takes the position then. The inputs register themselves; the screen
// (AppView) runs them for each action whose context passes their field, under the field's own
// name (the server's check of a screen makes sure of that, internal/apps/catalog.go).

import { createContext } from 'react'
import { submissionKey } from './operations'

export type SubmitResult = {
  // Put in the action's context under the field's name.
  value?: unknown
  // The action isn't sent: why, in the person's words.
  block?: string
}

export type SubmitHook = () => Promise<SubmitResult>

type Pressed = { name: string; context?: Record<string, unknown> } & Record<string, unknown>

export class SubmitCaptures {
  private readonly hooks = new Map<string, SubmitHook>()
  // The submissions being made, by what identifies them: a second press of one is that one.
  private readonly flying = new Map<string, Promise<void>>()
  private readonly listeners = new Set<() => void>()

  // Registers the hook of field; returns what removes it.
  register(field: string, hook: SubmitHook): () => void {
    this.hooks.set(field, hook)
    return () => {
      if (this.hooks.get(field) === hook) this.hooks.delete(field)
    }
  }

  // What identifies a submission: the action as pressed, without the fields taken as it is
  // submitted (which are what varies from one press to the next).
  key(action: Pressed): string {
    const context = { ...(action.context ?? {}) }
    for (const field of this.hooks.keys()) delete context[field]
    const { timestamp: _timestamp, ...rest } = action
    return submissionKey('captures', { ...rest, context })
  }

  // Whether a submission is being made: its capture is pending or its request is on its way.
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

  // Makes a submission once at a time: while it is pending (a location being taken, the request on
  // its way) pressing it again is the same submission and gets its answer, not a second one.
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

  // Runs the hooks of the fields the context passes. Returns the context to send, or why the action
  // must not be sent. What a hook takes stays in the form (the input holds it), so pressing the same
  // submission again sends the same values and, through them, the same operation (operations.ts).
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
