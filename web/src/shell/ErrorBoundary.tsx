// Keeps a failure where it happened: what is drawn from content the platform doesn't control (an app's
// widget, a pin's label) or from an answer that may be malformed is wrapped in one, so a throw while
// rendering it shows the fallback in its place and the rest of the page, and the shell around it,
// carry on. The fallback goes away when resetKey changes (another page, a reload).

import { Component, type ErrorInfo, type ReactNode } from 'react'

type Props = { fallback: ReactNode; resetKey?: unknown; children: ReactNode }
type State = { failed: boolean; resetKey: unknown }

export class ErrorBoundary extends Component<Props, State> {
  state: State = { failed: false, resetKey: this.props.resetKey }

  static getDerivedStateFromError(): Partial<State> {
    return { failed: true }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    return props.resetKey !== state.resetKey ? { failed: false, resetKey: props.resetKey } : null
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    // For whoever opens the console; people are told in the fallback.
    console.error('rendering failed:', error, info.componentStack)
  }

  render() {
    return this.state.failed ? this.props.fallback : this.props.children
  }
}
