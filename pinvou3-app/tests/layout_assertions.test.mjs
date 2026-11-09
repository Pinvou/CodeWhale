import test from 'node:test';
import assert from 'node:assert/strict';
import { assertStableLayoutHeight, LAYOUT_HEIGHT_ERROR_PX } from './helpers/layout-assertions.mjs';

test('layout height accepts identical samples and the recorded DOMRect rounding difference', () => {
  assert.equal(LAYOUT_HEIGHT_ERROR_PX, 0.01);
  for (const [before, after] of [[50, 50], [50.18743896484375, 50.1875], [50.1875, 50.18743896484375], [50, 50.005]]) {
    assert.doesNotThrow(() => assertStableLayoutHeight(before, after, 'member row'));
  }
});

test('layout height still rejects growth and shrinkage above the fixed CSS-pixel allowance', () => {
  for (const delta of [0.011, 0.02, 1, -0.011, -0.02, -1]) {
    assert.throws(() => assertStableLayoutHeight(50, 50 + delta, 'member row'), assert.AssertionError);
  }
});

test('layout height rejects non-numeric, non-finite and non-positive samples', () => {
  for (const value of [undefined, null, '50', NaN, Infinity, -Infinity, 0, -1]) {
    assert.throws(() => assertStableLayoutHeight(value, 50, 'member row'), /invalid layout height/);
    assert.throws(() => assertStableLayoutHeight(50, value, 'member row'), /invalid layout height/);
  }
});

test('layout height failure retains the comparison context and fixed threshold', () => {
  assert.throws(() => assertStableLayoutHeight(50, 51, 'member row after opening menu'), error => {
    assert.match(error.message, /member row after opening menu/);
    assert.match(error.message, /before=50, after=51, delta=1, allowed=0\.01 CSS px/);
    return error instanceof assert.AssertionError;
  });
});
