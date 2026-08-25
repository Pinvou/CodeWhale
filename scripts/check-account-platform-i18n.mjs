#!/usr/bin/env node

import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { accountPlatformCopy } from '../pinvou3-app/src/shared/account-platform-i18n.js';
import { adminPlatformCopy } from '../pinvou3-app/src/shared/admin-platform-i18n.js';
import { supportedLocales } from '../pinvou3-app/src/shared/supported-locales.js';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const portalAssets = path.join(repositoryRoot, 'account-platform', 'internal', 'portal', 'static', 'assets');
const identityThemeMessages = path.join(repositoryRoot, 'account-platform', 'deploy', 'keycloak', 'themes', 'pinvou', 'login', 'messages');
const userInterfaceSources = [
  'pinvou3-app/src/features/account-platform/AccountPlatformPopover.jsx',
  'pinvou3-app/src/features/account-platform/AccountPlatformView.jsx',
  'pinvou3-app/src/features/account-platform/AdminPlatformView.jsx',
  'pinvou3-app/src/app/account-main.jsx',
  'pinvou3-app/src/app/admin-main.jsx',
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

async function builtJavaScript() {
  const names = (await readdir(portalAssets)).filter(name => name.endsWith('.js'));
  return (await Promise.all(names.map(name => readFile(path.join(portalAssets, name), 'utf8')))).join('\n');
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
