#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { accountPlatformCopy } from '../pinvou3-app/src/shared/account-platform-i18n.js';
import { adminPlatformCopy } from '../pinvou3-app/src/shared/admin-platform-i18n.js';
import { supportedLocales } from '../pinvou3-app/src/shared/supported-locales.js';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const portalStatic = path.join(repositoryRoot, 'account-platform', 'internal', 'portal', 'static');
const identityThemeMessages = path.join(repositoryRoot, 'account-platform', 'deploy', 'keycloak', 'themes', 'pinvou', 'login', 'messages');
const userInterfaceSources = [
  'pinvou3-app/src/features/account-platform/AccountPlatformPopover.jsx',
  'pinvou3-app/src/features/account-platform/AccountPlatformView.jsx',
  'pinvou3-app/src/features/account-platform/AdminPlatformView.jsx',
  'pinvou3-app/src/app/account-main.jsx',
  'pinvou3-app/src/app/admin-main.jsx',
  'pinvou3-app/src/portal-session-handoff.html',
  'pinvou3-app/src/app/portal-session-handoff-main.js',
];

function kind(value) {
  if (Array.isArray(value)) return 'array';
  if (value !== null && typeof value === 'object') return 'object';
  return typeof value;
}

function visit(reference, candidate, label, strings) {
  const referenceKind = kind(reference);
  if (kind(candidate) !== referenceKind) throw new Error(`i18n_type_mismatch:${label}`);
  if (referenceKind === 'string') {
    if (!reference.trim() || !candidate.trim()) throw new Error(`i18n_empty_translation:${label}`);
    strings.add(candidate);
    return;
  }
  if (referenceKind === 'function') return;
  if (referenceKind === 'array') {
    if (reference.length !== candidate.length) throw new Error(`i18n_array_length_mismatch:${label}`);
    reference.forEach((item, index) => visit(item, candidate[index], `${label}.${index}`, strings));
    return;
  }
  if (referenceKind !== 'object') return;
  const referenceKeys = Object.keys(reference).sort();
  const candidateKeys = Object.keys(candidate).sort();
  if (referenceKeys.join('\0') !== candidateKeys.join('\0')) {
    const missing = referenceKeys.filter(key => !candidateKeys.includes(key));
    const extra = candidateKeys.filter(key => !referenceKeys.includes(key));
    throw new Error(`i18n_key_mismatch:${label}:missing=${missing.join(',')}:extra=${extra.join(',')}`);
  }
  for (const key of referenceKeys) visit(reference[key], candidate[key], `${label}.${key}`, strings);
}

export function validateCatalog(name, catalog) {
  const locales = Object.keys(catalog).sort();
  const expected = [...supportedLocales].sort();
  if (locales.join('\0') !== expected.join('\0')) throw new Error(`i18n_locale_mismatch:${name}`);
  const strings = new Set();
  for (const locale of supportedLocales) visit(catalog.zh, catalog[locale], `${name}.${locale}`, strings);
  return strings;
}

function stripComments(source) {
  let result = '';
  let quote = '';
  let lineComment = false;
  let blockComment = false;
  for (let index = 0; index < source.length; index += 1) {
    const char = source[index];
    const next = source[index + 1];
    if (lineComment) {
      if (char === '\n') { lineComment = false; result += char; }
      continue;
    }
    if (blockComment) {
      if (char === '*' && next === '/') { blockComment = false; index += 1; }
      continue;
    }
    if (quote) {
      result += char;
      if (char === '\\') { result += next || ''; index += 1; }
      else if (char === quote) quote = '';
      continue;
    }
    if (char === "'" || char === '"' || char === '`') { quote = char; result += char; continue; }
    if (char === '/' && next === '/') { lineComment = true; index += 1; continue; }
    if (char === '/' && next === '*') { blockComment = true; index += 1; continue; }
    result += char;
  }
  return result;
}

export function validateUserInterfaceSource(relative, source) {
  const active = stripComments(source);
  if (/[一-鿿぀-ヿ]/u.test(active)) throw new Error(`i18n_single_language_literal:${relative}`);
  if (/(?:label|title|placeholder|aria-label)\s*[:=]\s*['"][A-Za-z][^'"]*[a-z][^'"]*['"]/u.test(active)) {
    throw new Error(`i18n_single_language_property:${relative}`);
  }
  if (/reason\?\.message\s*&&|\?\s*reason\.message\s*:/u.test(active)) {
    throw new Error(`i18n_raw_error_fallback:${relative}`);
  }
}

export async function builtJavaScript(root = portalStatic) {
  const pending = [];
  for (const entry of ['account.html', 'admin.html', 'portal-session-handoff.html']) {
    const html = await readFile(path.join(root, entry), 'utf8');
    for (const match of html.matchAll(/(?:src|href)=["']\/assets\/([^"']+\.js)["']/g)) pending.push(match[1]);
  }
  const seen = new Set();
  const sources = [];
  while (pending.length) {
    const name = pending.pop();
    if (!/^[A-Za-z0-9_.-]+\.js$/.test(name)) throw new Error('i18n_artifact_path_invalid');
    if (seen.has(name)) continue;
    seen.add(name);
    const source = await readFile(path.join(root, 'assets', name), 'utf8');
    sources.push(source);
    for (const match of source.matchAll(/["']\.\/([^"']+\.js)["']/g)) pending.push(match[1]);
  }
  if (!seen.size) throw new Error('i18n_artifact_entry_missing');
  return sources.join('\n');
}

export function validateBuiltCatalog(name, strings, artifact) {
  for (const value of strings) {
    if (!artifact.includes(value)) throw new Error(`i18n_artifact_translation_missing:${name}:${value.slice(0, 40)}`);
  }
}

function parseProperties(source, label) {
  const values = new Map();
  for (const [index, line] of source.split(/\r?\n/).entries()) {
    if (!line.trim() || line.trimStart().startsWith('#')) continue;
    const separator = line.indexOf('=');
    if (separator <= 0) throw new Error(`i18n_properties_invalid:${label}:${index + 1}`);
    const key = line.slice(0, separator).trim();
    const value = line.slice(separator + 1).trim();
    if (!value) throw new Error(`i18n_empty_translation:${label}.${key}`);
    if (values.has(key)) throw new Error(`i18n_duplicate_key:${label}.${key}`);
    values.set(key, value);
  }
  return values;
}

async function validateIdentityTheme() {
  const files = { zh: 'messages_zh_CN.properties', en: 'messages_en.properties', ja: 'messages_ja.properties' };
  const catalogs = Object.fromEntries(await Promise.all(Object.entries(files).map(async ([locale, file]) => [locale, parseProperties(await readFile(path.join(identityThemeMessages, file), 'utf8'), `identity.${locale}`)])));
  const reference = [...catalogs.zh.keys()].sort();
  for (const [locale, catalog] of Object.entries(catalogs)) {
    const keys = [...catalog.keys()].sort();
    if (keys.join('\0') !== reference.join('\0')) throw new Error(`i18n_key_mismatch:identity.${locale}`);
  }
}

export async function checkAccountPlatformI18n({ artifacts = true } = {}) {
  const catalogs = [
    ['account', accountPlatformCopy],
    ['admin', adminPlatformCopy],
  ];
  const validated = catalogs.map(([name, catalog]) => [name, validateCatalog(name, catalog)]);
  // The canonical handoff entry consumes this catalog directly. Vite emits
  // hashed chunks; no separately named handoff translation module exists.
  validateCatalog('handoff', Object.fromEntries(supportedLocales.map(locale => [locale, accountPlatformCopy[locale].sessionHandoff])));
  const handoff = await readFile(path.join(repositoryRoot, 'pinvou3-app/src/app/portal-session-handoff-main.js'), 'utf8');
  if (!handoff.includes("import { accountPlatformCopy } from '../shared/i18n.js'")
    || !handoff.includes('const copy = accountPlatformCopy[locale]')
    || !handoff.includes('copy.sessionHandoff.')) throw new Error('i18n_handoff_source_mismatch');
  await validateIdentityTheme();
  for (const relative of userInterfaceSources) {
    validateUserInterfaceSource(relative, await readFile(path.join(repositoryRoot, relative), 'utf8'));
  }
  if (artifacts) {
    const artifact = await builtJavaScript();
    for (const [name, strings] of validated) validateBuiltCatalog(name, strings, artifact);
  }
  return { catalogs: validated.length + 1, sources: userInterfaceSources.length };
}

const invokedPath = process.argv[1] ? path.resolve(process.argv[1]) : '';
if (invokedPath === fileURLToPath(import.meta.url)) {
  const result = await checkAccountPlatformI18n({ artifacts: !process.argv.includes('--source-only') });
  process.stdout.write(`Account Platform i18n gate passed (${result.catalogs} catalogs, ${result.sources} UI sources).\n`);
}
