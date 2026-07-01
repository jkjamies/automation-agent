/**
 * A simple chat REPL over the configured model, for local development only.
 *
 * Launched via `make playground`. Development only, never part of a deployed artifact.
 * Swap in the summary / lintfixer / covfixer agents here to drive the real workflows
 * interactively.
 */
import { createInterface } from 'node:readline/promises';
import { LlmAgent } from '@google/adk';

import { buildLLM } from '../../src/agent/setup/llm';
import { driveText, newRunner } from '../../src/agent/setup/runner';
import { TracesExporter, load } from '../../src/config/config';
import * as obs from '../../src/obs/index';

async function main(): Promise<void> {
  try {
    process.loadEnvFile('.env');
  } catch {
    // no .env — fine
  }
  const cfg = load();

  // Default the playground to the console exporter so a developer sees the span tree on stdout with
  // no backend to stand up — but respect an explicit OTEL_TRACES_EXPORTER (config records whether
  // one was provided, so this file never reads the environment itself).
  const exporter = cfg.otelTracesExporterSet ? cfg.otelTracesExporter : TracesExporter.Console;
  const shutdownObs = await obs.init({
    exporter,
    serviceName: cfg.otelServiceName,
    otlpEndpoint: cfg.otelExporterOtlpEndpoint,
    otlpHeaders: cfg.otelExporterOtlpHeaders,
    sampler: cfg.otelTracesSampler,
  });

  const agent = new LlmAgent({
    name: 'automation_agent_playground',
    description: 'Local playground for poking the configured model.',
    model: buildLLM(cfg),
    instruction:
      `You are the automation-agent local playground, backed by the configured ` +
      `model. Help the developer test prompts. Be concise.`,
  });
  const runner = newRunner('playground', agent);

  const rl = createInterface({ input: process.stdin, output: process.stdout });
  console.log("automation-agent playground — type a prompt, or 'exit' to quit.");
  const sessionId = 'playground';
  try {
    for (;;) {
      const line = (await rl.question('> ')).trim();
      if (line === 'exit' || line === 'quit') {
        break;
      }
      if (line === '') {
        continue;
      }
      try {
        const out = await driveText(runner, 'dev', sessionId, line);
        console.log(out);
      } catch (err) {
        console.error(`error: ${(err as Error).message}`);
      }
    }
  } finally {
    rl.close();
    // Force-flush buffered spans before the dev process ends.
    await shutdownObs();
  }
}

main().catch((err) => {
  console.error(`fatal: ${(err as Error).message}`);
  process.exitCode = 1;
});
