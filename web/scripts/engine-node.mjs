// Run a Node entry point under an interpreter that satisfies the engines.node
// range this package declares.
//
// npm prepends every ancestor node_modules/.bin directory to PATH before it
// runs a script, so an unrelated `node` package installed anywhere above the
// checkout shadows the interpreter npm itself is running with — `vitest`'s
// `#!/usr/bin/env node` shebang then picks the shadow. When that shadow is a
// Node 20 release, jsdom's undici dependency fails to load because
// worker_threads.markAsUncloneable only exists from Node 22.10, and vitest
// reports `webidl.util.markAsUncloneable is not a function` once per spec file:
// dozens of identical errors naming neither Node nor the version wanted.
//
// npm_node_execpath is the interpreter npm is running with, which is the one
// the developer actually selected, so re-exec this wrapper under it and let the
// check run again against that interpreter's real process.version. Nothing here
// relaxes a test: the target still runs, and its exit code is the exit code.
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const selfPath = fileURLToPath(import.meta.url);
const webRoot = resolve(dirname(selfPath), '..');
// Set on the re-executed child so a second mismatch reports instead of
// spawning interpreters forever.
const reexecMarker = 'POSTRA_ENGINE_NODE_REEXEC';

// Only the `>=major.minor.patch` shape this package uses is understood; any
// other range is left alone rather than guessed at.
export function parseMinimumVersion(range) {
  const match = /^>=\s*(\d+)(?:\.(\d+))?(?:\.(\d+))?$/.exec(String(range ?? '').trim());
  if (!match) return null;
  return [Number(match[1]), Number(match[2] ?? 0), Number(match[3] ?? 0)];
}

export function meetsMinimum(version, minimum) {
  const match = /^v?(\d+)\.(\d+)\.(\d+)/.exec(String(version ?? '').trim());
  if (!match || !minimum) return false;
  const found = [Number(match[1]), Number(match[2]), Number(match[3])];
  for (let index = 0; index < 3; index++) {
    if (found[index] !== minimum[index]) return found[index] > minimum[index];
  }
  return true;
}

// Decide what to do with the interpreter currently running this file. Pure, so
// the shadowed-PATH branches are exercised without a second Node installed.
export function planInterpreter({ range, version, execPath, npmExecPath, alreadyReexeced }) {
  const minimum = parseMinimumVersion(range);
  if (minimum === null) return { action: 'run', reason: `cannot read engines.node range "${range}"; running ${version} as is` };
  if (meetsMinimum(version, minimum)) return { action: 'run' };
  const required = minimum.join('.');
  if (npmExecPath && npmExecPath !== execPath && !alreadyReexeced) {
    return {
      action: 'reexec', execPath: npmExecPath,
      reason: `node ${version} at ${execPath} is older than the required >=${required}; retrying with the interpreter npm is running (${npmExecPath})`,
    };
  }
  return {
    action: 'fail',
    reason: `node ${version} at ${execPath} cannot run this package: package.json requires node >=${required}.\n`
      + '  npm puts every ancestor node_modules/.bin on PATH, so a `node` package installed above this\n'
      + '  checkout can shadow the node you selected. Remove it, or put a supported node earlier on PATH.',
  };
}

function exitCodeOf(result) {
  if (result.error) { console.error(`engine-node: ${result.error.message}`); return 1; }
  if (result.signal) { console.error(`engine-node: target killed by ${result.signal}`); return 1; }
  return result.status ?? 1;
}

function main(argv) {
  const [target, ...targetArgs] = argv;
  if (!target) { console.error('usage: node scripts/engine-node.mjs <entry.mjs> [args...]'); return 2; }
  const manifest = JSON.parse(readFileSync(resolve(webRoot, 'package.json'), 'utf8'));
  const plan = planInterpreter({
    range: manifest.engines?.node, version: process.version, execPath: process.execPath,
    npmExecPath: process.env[reexecMarker] ? undefined : process.env.npm_node_execpath,
    alreadyReexeced: Boolean(process.env[reexecMarker]),
  });
  if (plan.reason) console.error(`engine-node: ${plan.reason}`);
  if (plan.action === 'fail') return 1;
  if (plan.action === 'reexec') {
    return exitCodeOf(spawnSync(plan.execPath, [selfPath, ...argv], {
      stdio: 'inherit', env: { ...process.env, [reexecMarker]: '1' },
    }));
  }
  return exitCodeOf(spawnSync(process.execPath, [resolve(webRoot, target), ...targetArgs], { stdio: 'inherit' }));
}

// Importing this file for its decision helpers must not spawn anything.
if (process.argv[1] && resolve(process.argv[1]) === selfPath) process.exit(main(process.argv.slice(2)));
