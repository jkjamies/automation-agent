/**
 * The dispatcher kicked off for every ingest.
 *
 * It routes a normalized {@link Envelope} to the right workflow by {@link Kind}. Keeping
 * a single entry point is why "root" exists: new ingress sources and smarter (e.g.
 * LLM-based) routing slot in here without restructuring.
 */
import { type Envelope, type Kind } from '../../ingest/envelope';

/** Runs the work for one ingest envelope; errors raise instead of being returned. */
export type Handler = (e: Envelope) => Promise<void>;

/** Structured logger the dispatcher emits through (optional). */
export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
}

/** Routes envelopes to handlers by Kind. */
export class Dispatcher {
  private readonly handlers = new Map<Kind, Handler>();

  constructor(private readonly log?: Logger | null) {}

  /** Bind a handler to a kind (last registration wins). */
  register(kind: Kind, handler: Handler): void {
    this.handlers.set(kind, handler);
  }

  /** Report whether a kind has a registered handler. */
  handles(kind: Kind): boolean {
    return this.handlers.has(kind);
  }

  /**
   * Route one envelope. An unregistered kind is logged and ignored, so an ingress that
   * isn't wired yet is a no-op, not a crash.
   */
  async dispatch(e: Envelope): Promise<void> {
    const handler = this.handlers.get(e.kind);
    if (!handler) {
      this.log?.warn('no handler for ingest kind', { kind: e.kind, source: e.source });
      return;
    }
    this.log?.info('dispatching', { kind: e.kind, source: e.source });
    await handler(e);
  }
}
