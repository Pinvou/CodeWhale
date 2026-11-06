import assert from 'node:assert/strict';

// DOMRect coordinates can differ by subpixel rounding, including under a
// transform. This fixed CSS-pixel allowance is 100 times smaller than 1px;
// it is only for layout measurements, not domain or financial values.
export const LAYOUT_HEIGHT_ERROR_PX = 0.01;

export function assertStableLayoutHeight(before, after, description) {
  assert.ok(Number.isFinite(before) && before > 0 && Number.isFinite(after) && after > 0,
    `invalid layout height: before=${before}, after=${after}`);
  const delta = Math.abs(after - before);
  assert.ok(delta <= LAYOUT_HEIGHT_ERROR_PX,
    `${description}: before=${before}, after=${after}, delta=${delta}, allowed=${LAYOUT_HEIGHT_ERROR_PX} CSS px`);
}
