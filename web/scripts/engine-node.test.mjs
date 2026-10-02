// The `test` script runs through scripts/engine-node.mjs because npm prepends
// every ancestor node_modules/.bin directory to PATH, so an unrelated `node`
// package installed above the checkout shadows the interpreter npm itself runs
// with. These pin the interpreter decision and the wrapper's process wiring.
import { describe, expect, it } from 'vitest';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { meetsMinimum, parseMinimumVersion, planInterpreter } from './engine-node.mjs';

const scriptsRoot = dirname(fileURLToPath(import.meta.url));
const wrapper = join(scriptsRoot, 'engine-node.mjs');
const webRoot = resolve(scriptsRoot, '..');
// Read the range the package really declares so these stay true after a bump.
const declaredRange = JSON.parse(readFileSync(join(webRoot, 'package.json'), 'utf8')).engines.node;

describe('parseMinimumVersion', () => {
  it('reads the minimum out of the range package.json declares', () => {
    expect(parseMinimumVersion(declaredRange)).toEqual([22, 12, 0]);
  });

  it('returns null for a range it cannot interpret', () => {
    expect(parseMinimumVersion('^22 || 20')).toBeNull();
  });
});

describe('meetsMinimum', () => {
  const minimum = parseMinimumVersion(declaredRange);

  // 20.19.2 is the version the npm `node` package shadowed this checkout with;
  // jsdom's undici needs worker_threads.markAsUncloneable, added in 22.10.
  it('rejects the shadowing node 20 release', () => {
    expect(meetsMinimum('v20.19.2', minimum)).toBe(false);
  });

  it('accepts a current node 22 release', () => {
    expect(meetsMinimum('v22.23.1', minimum)).toBe(true);
  });

  it('accepts exactly the declared minimum and rejects the release below it', () => {
    expect(meetsMinimum('v22.12.0', minimum)).toBe(true);
    expect(meetsMinimum('v22.11.9', minimum)).toBe(false);
  });
});

describe('planInterpreter', () => {
  const shadowed = {
    range: declaredRange,
    version: 'v20.19.2',
    execPath: '/home/someone/node_modules/node/bin/node',
    npmExecPath: '/home/someone/.nvm/versions/node/v22.23.1/bin/node',
    alreadyReexeced: false,
  };

  it('runs the target when the running interpreter satisfies the range', () => {
    expect(planInterpreter({ ...shadowed, version: process.version })).toMatchObject({ action: 'run' });
  });

  it('re-executes under the interpreter npm is running with when PATH shadowed it', () => {
    const plan = planInterpreter(shadowed);
    expect(plan.action).toBe('reexec');
    expect(plan.execPath).toBe(shadowed.npmExecPath);
  });

  it('reports instead of re-executing forever when the retry is also too old', () => {
    const plan = planInterpreter({ ...shadowed, alreadyReexeced: true });
    expect(plan.action).toBe('fail');
    // The failure has to name both versions; the undici error named neither.
    expect(plan.reason).toContain('v20.19.2');
    expect(plan.reason).toContain('22.12.0');
  });

  it('reports when npm offers no interpreter of its own', () => {
    expect(planInterpreter({ ...shadowed, npmExecPath: undefined })).toMatchObject({ action: 'fail' });
  });

  it('runs the target rather than guessing when the range is uninterpretable', () => {
    expect(planInterpreter({ ...shadowed, range: '^22 || 20' })).toMatchObject({ action: 'run' });
  });
});

describe('the wrapper process', () => {
  it('runs its target with the remaining arguments and propagates the exit code', () => {
    const directory = mkdtempSync(join(tmpdir(), 'postra-engine-node-'));
    try {
      const target = join(directory, 'target.mjs');
      writeFileSync(target, 'console.log(JSON.stringify(process.argv.slice(2)));process.exit(7);\n');
      const result = spawnSync(process.execPath, [wrapper, target, 'run', '--reporter=dot'], { encoding: 'utf8' });
      expect(result.stdout.trim()).toBe('["run","--reporter=dot"]');
      expect(result.status).toBe(7);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  it('fails with a usage line when given no target', () => {
    const result = spawnSync(process.execPath, [wrapper], { encoding: 'utf8' });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('usage:');
  });
});
