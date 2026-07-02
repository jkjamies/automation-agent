/**
 * The in-house PR code-review workflow (a CodeRabbit-style advisory reviewer).
 *
 * It reacts to GitHub `pull_request` events (routed as {@link Kind.Review}) and produces a
 * count-based scorecard from per-category sub-agent findings. It is comment-only and never opens
 * PRs. Publishing the scored review to the PR is a follow-up.
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
