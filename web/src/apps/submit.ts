// What a form does as it is submitted, before its action is sent: a location field whose
// capture is "submit" takes the position then. The inputs register themselves; the screen
// (AppView) runs them for each action whose context passes their field, under the field's own
// name (the server's check of a screen makes sure of that, internal/apps/catalog.go).

import { createContext } from 'react'

export type SubmitResult = {
  // Put in the action's context under the field's name.
  value?: unknown
  // The action isn't sent: why, in the person's words.
  block?: string
}

export type SubmitHook = () => Promise<SubmitResult>

export class SubmitCaptures {
  private readonly hooks = new Map<string, SubmitHook>()

  // Registers the hook of field; returns what removes it.
  register(field: string, hook: SubmitHook): () => void {
    this.hooks.set(field, hook)
    return () => {
      if (this.hooks.get(field) === hook) this.hooks.delete(field)
    }
  }

  // Runs the hooks of the fields the context passes. Returns the context to send, or why the action
  // must not be sent.
  async apply(
    context: Record<string, unknown> | undefined,
  ): Promise<{ context?: Record<string, unknown>; block?: string }> {
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
