/**
 * A boundary around one panel, so a broken panel is a broken panel.
 *
 * # Why this is per panel and not per page
 *
 * The grid's whole value is that several projects are visible at once, and the
 * failure this guards against is one of them going wrong. React unmounts the
 * whole tree above a boundary when an error escapes, so a boundary at the page
 * would turn one terminal's exception into a white workspace - losing the four
 * terminals that were fine, and the scrollback in them that only exists in the
 * browser.
 *
 * # Retry is not restart
 *
 * "Try again" remounts the panel, which re-subscribes and takes a fresh screen.
 * It does not touch the runtime, the session, or the agent. That is the same
 * distinction the terminal's own Redraw makes, and it is the one that matters:
 * a rendering failure is never a reason to end somebody's work.
 */
import { Component, type ErrorInfo, type ReactNode } from 'react'

interface PanelBoundaryProps {
  /** What is inside, named for the message: "Project AgentMux failed". */
  label: string
  children: ReactNode
}

interface PanelBoundaryState {
  error: Error | null
}

export class PanelBoundary extends Component<PanelBoundaryProps, PanelBoundaryState> {
  override state: PanelBoundaryState = { error: null }

  static getDerivedStateFromError(error: Error): PanelBoundaryState {
    return { error }
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    // The console is the only place this can go: there is no server endpoint
    // for a client-side crash, and inventing one would be a new place a
    // terminal's contents could end up. The message and the component stack
    // are what a person pasting this into a bug report needs.
    console.error(`[AgentMux] ${this.props.label} failed`, error, info.componentStack)
  }

  private readonly retry = (): void => {
    this.setState({ error: null })
  }

  override render(): ReactNode {
    const { error } = this.state
    if (!error) return this.props.children

    return (
      <section className="panel panel--failed" aria-label={`${this.props.label} failed`}>
        <div className="panel__body">
          <p className="panel__placeholder-title">This panel failed to render</p>
          <p className="panel__placeholder-text">
            {error.message || 'The panel stopped with an error.'}
          </p>
          <p className="panel__placeholder-text">
            The project itself is untouched — its terminal is still running, and its work is still
            there.
          </p>
          <div className="panel__placeholder-actions">
            <button type="button" className="button" onClick={this.retry}>
              Try again
            </button>
          </div>
        </div>
      </section>
    )
  }
}
