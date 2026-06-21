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
import { load } from '../../src/config/config';

async function main(): Promise<void> {
  try {
    process.loadEnvFile('.env');
  } catch {
    // no .env — fine
  }
  const cfg = load();

  const agent = new LlmAgent({
    name: 'automation_agent_playground',
    description: 'Local playground for poking the configured model.',
    model: buildLLM(cfg),
    instruction:
      `You are the automation-agent local playground, backed by the model ` +
      `'${cfg.ollamaModel}'. Help the developer test prompts. Be concise.`,
  });
  const runner = newRunner('playground', agent);

  const rl = createInterface({ input: process.stdin, output: process.stdout });
  console.log("automation-agent playground — type a prompt, or 'exit' to quit.");
  const sessionId = 'playground';
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
  rl.close();
}

main().catch((err) => {
  console.error(`fatal: ${(err as Error).message}`);
  process.exitCode = 1;
});
