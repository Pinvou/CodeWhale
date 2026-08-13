import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import {
  relayAvatarAssetSize,
  relayAvatarContrastRatio,
  relayAvatarFallback,
  relayAvatarTone,
} from '../src/features/team-relay/relay-avatar.mjs';

test('Team Relay fallback avatars use one readable name character', () => {
  assert.equal(relayAvatarFallback('刘洋'), '刘');
  assert.equal(relayAvatarFallback('alice'), 'A');
  assert.equal(relayAvatarFallback('  bob'), 'B');
  assert.equal(relayAvatarFallback('·陈晨'), '陈');
  assert.equal(relayAvatarFallback(''), '?');
});

test('Team Relay fallback colors are stable per person and independent from expert avatars', () => {
  assert.deepEqual(relayAvatarTone('principal-a'), relayAvatarTone('principal-a'));
  assert.notDeepEqual(relayAvatarTone('principal-a'), relayAvatarTone('principal-b'));
  const consecutivePeople = Array.from(
    { length: 100 },
    (_, index) => JSON.stringify(relayAvatarTone(`relay-member-${String(index).padStart(3, '0')}`)),
  );
  assert.equal(new Set(consecutivePeople).size, consecutivePeople.length);
  const component = fs.readFileSync(
    new URL('../src/features/team-relay/RelayAvatar.jsx', import.meta.url),
    'utf8',
  );
  assert.doesNotMatch(component, /expert-avatar|expertAvatarSrc/);
  assert.doesNotMatch(component, /backgroundColor|color:\s*tone\.foreground/, 'avatar colors must flow through the shared CSS variables');
});

test('Team Relay member styles preserve stable avatar colors and mark self without recoloring', () => {
  const stylesheet = fs.readFileSync(
    new URL('../src/features/team-relay/team-relay.css', import.meta.url),
    'utf8',
  );
  const memberRules = [...stylesheet.matchAll(/\.team-relay-member-avatar\s*\{([^}]*)\}/g)];
  const colorRule = memberRules.find(([, declarations]) => /(?:^|;)\s*background\s*:/.test(declarations));
  assert.ok(colorRule, 'member avatars must define their fallback colors');
  assert.match(colorRule[1], /background\s*:\s*var\(--relay-avatar-bg\)/);
  assert.match(colorRule[1], /(?:^|;)\s*color\s*:\s*var\(--relay-avatar-fg\)/);
  const selfRule = stylesheet.match(/\.team-relay-member-avatar\.is-me\s*\{([^}]*)\}/);
  assert.ok(selfRule, 'the current member must have a visible marker');
  assert.match(selfRule[1], /box-shadow\s*:/);
  assert.doesNotMatch(selfRule[1], /(?:^|;)\s*(?:background|color)\s*:/, 'the current member marker must not replace the stable palette');
  const profileRule = stylesheet.match(/\.team-relay-profile-button\s*>\s*span\s*\{([^}]*)\}/);
  assert.ok(profileRule, 'the header avatar must define its compact presentation');
  assert.match(profileRule[1], /background\s*:\s*var\(--relay-avatar-bg\)/);
  assert.match(profileRule[1], /(?:^|;)\s*color\s*:\s*var\(--relay-avatar-fg\)/);
});

test('Team Relay fallback colors meet WCAG AA contrast for a broad stable palette', () => {
  let minimumContrast = Number.POSITIVE_INFINITY;
  for (let index = 0; index < 250_000; index += 1) {
    const tone = relayAvatarTone(`relay-contrast-${index}`);
    minimumContrast = Math.min(
      minimumContrast,
      relayAvatarContrastRatio(tone.background, tone.foreground),
    );
  }
  assert.ok(minimumContrast >= 4.5, `minimum contrast was ${minimumContrast}`);
});

test('Team Relay requests compact animated avatar variants for rendered density', () => {
  assert.equal(relayAvatarAssetSize(24, 1), 48);
  assert.equal(relayAvatarAssetSize(24, 2), 48);
  assert.equal(relayAvatarAssetSize(28, 2), 96);
  assert.equal(relayAvatarAssetSize(42, 2), 96);
  const component = fs.readFileSync(
    new URL('../src/features/team-relay/RelayAvatar.jsx', import.meta.url),
    'utf8',
  );
  const view = fs.readFileSync(
    new URL('../src/features/team-relay/TeamRelayView.jsx', import.meta.url),
    'utf8',
  );
  assert.match(component, /requestedDisplaySize/);
  assert.match(component, /getBoundingClientRect/);
  assert.match(component, /typeof window === 'undefined'/);
  assert.match(component, /const useOriginal = displaySize === null/);
  assert.match(view, /team-relay-profile-avatar-preview[^>]+displaySize=\{null\}/);
  assert.match(component, /let avatarIntersectionObserver = null/);
  assert.match(component, /rootMargin: '200px'/);
  assert.match(component, /avatarIntersectionObserver\?\.unobserve\(entry\.target\)/);
  assert.match(component, /if \(!browserWindow\?\.IntersectionObserver\) \{\s*ready\(\)/);
  assert.match(component, /if \(!ready\) return/);
  assert.match(component, /loading="lazy"/);
  assert.match(component, /decoding="async"/);
  assert.match(component, /\{!source && <span>\{relayAvatarFallback\(name\)\}<\/span>\}/, 'transparent custom avatars must not reveal the fallback glyph underneath');
  assert.match(component, /Promise\.resolve\(\)\s*\.then\(\(\) => active \? loadSource/);
  assert.match(component, /if \(active\) setSource/);
  assert.match(component, /return \(\) => \{ active = false; \}/);
  assert.doesNotMatch(component, /animationPlayState|\.pause\(/);
});
