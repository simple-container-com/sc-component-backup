import { Component, type ErrorInfo, type ReactNode } from "react";

interface State { error: Error | null }

export class ErrorBoundary extends Component<{ children: ReactNode; resetKey?: string }, State> {
  state: State = { error: null };
  static getDerivedStateFromError(error: Error): State {
    return { error };
  }
  componentDidUpdate(prev: { resetKey?: string }) {
    if (prev.resetKey !== this.props.resetKey && this.state.error) this.setState({ error: null });
  }
  componentDidCatch(error: Error, info: ErrorInfo) {
    // Only the error and component stack — never form values.
    console.error("UI error:", error.message, info.componentStack);
  }
  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div role="alert" className="mx-auto mt-16 max-w-md rounded-lg border border-line bg-surface p-6">
        <h2 className="text-lg font-semibold">This screen failed to render</h2>
        <p className="mt-1 text-sm text-muted">Internal interface error. Your data and backups are not affected.</p>
        <pre className="mt-3 overflow-auto rounded bg-sunken p-2 font-mono text-xs text-muted">{this.state.error.message}</pre>
        <div className="mt-4 flex gap-2">
          <button className="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg" onClick={() => this.setState({ error: null })}>
            Try again
          </button>
          <button className="rounded border border-line px-3 py-1.5 text-sm" onClick={() => window.location.assign("/")}>
            Go to overview
          </button>
        </div>
      </div>
    );
  }
}
