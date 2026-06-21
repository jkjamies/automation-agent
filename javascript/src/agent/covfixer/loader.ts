/** The coverage-fixer's prompt loader (its own `prompts/` dir, reviewable next to it). */
import { fileURLToPath } from 'node:url';
import { dirname } from 'node:path';

import { Prompts } from '../setup/prompts';

export const prompts = new Prompts(dirname(fileURLToPath(import.meta.url)));
