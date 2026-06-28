/**
 * tasks — the execution transport between webhook ingress and the dispatcher.
 *
 * In-process (default, local) or Cloud Tasks (production, in-request). See
 * `specs/20260626-workflow-execution-transport.md`.
 */

export { type DispatchFunc, type EnqueueOptions, type Logger, type Transport } from './transport';
export { DEFAULT_MAX_CONCURRENT, DRAIN_TIMEOUT_MS, InProcess } from './inprocess';
export { CloudTasks, MAX_TASK_BYTES, type Submitter, newCloudTasks } from './cloudtasks';
