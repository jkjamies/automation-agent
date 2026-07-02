/**
 * The in-house PR code-review workflow (a CodeRabbit-style advisory reviewer).
 *
 * It reacts to GitHub `pull_request` events (routed as {@link Kind.Review}) and posts per-category
 * sub-agent findings, a count-based scorecard, inline comments with suggestions, and an advisory
 * `agent-review` check. Comment-only; it never opens PRs.
 */

export { enqueueOptions } from './enqueue';
export {
  type Decision,
  DecisionKind,
  type Deps,
  Engine,
  type GitHubClient,
  type Logger,
  isDependencyBot,
  newEngine,
  splitFullName,
} from './reviewer';
export { type Finding } from './findings';
