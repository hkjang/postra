// Bundle the notices shipped by locked production dependencies with /app.
// No network access and no generated timestamps: identical inputs stay stable.
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const lock = JSON.parse(readFileSync(join(webRoot, 'package-lock.json'), 'utf8'));
const sections = ['Postra browser application — third-party notices',
  'This file contains the license and copyright notices of the locked production dependencies. Not every package is necessarily included in the tree-shaken browser bundle.'];

for (const [packagePath, entry] of Object.entries(lock.packages).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)) {
  if (!packagePath || entry.dev) continue;
  const directory = join(webRoot, packagePath);
  const metadata = JSON.parse(readFileSync(join(directory, 'package.json'), 'utf8'));
  let candidates = readdirSync(directory)
    .filter(name => /^(licen[cs]e|copying|copyright|notice)/i.test(name))
    .map(name => join(directory, name))
    .filter(path => statSync(path).isFile());
  // Pretendard publishes its font license beside the distributable fonts.
  if (metadata.name === 'pretendard') candidates.push(join(directory, 'dist', 'LICENSE.txt'));
  // This locked npm package omits LICENSE from its published files; retain the
  // upstream notice locally so building the bundle never needs a network fetch.
  if (metadata.name === 'react-remove-scroll-bar') candidates.push(join(webRoot, 'licenses', 'react-remove-scroll-bar-LICENSE.txt'));
  if (!candidates.length) throw new Error(`Missing production license notice: ${metadata.name}@${metadata.version}`);
  const notices = [...new Set(candidates)].sort().map(path => readFileSync(path, 'utf8').replace(/\r\n/g, '\n').trim());
  sections.push(`${metadata.name}@${metadata.version}\nLicense: ${metadata.license || entry.license || 'See notice'}\n\n${notices.join('\n\n')}`);
}
writeFileSync(join(webRoot, '..', 'internal', 'transport', 'spa', 'assets', 'THIRD_PARTY_NOTICES.txt'), sections.join('\n\n' + '='.repeat(72) + '\n\n') + '\n');
