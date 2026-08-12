export function relayAvatarFallback(name) {
  const characters = Array.from(String(name || '').trim());
  const first = characters.find(character => /[\p{L}\p{N}]/u.test(character));
  if (!first) return '?';
  return /^[a-z]$/i.test(first) ? first.toUpperCase() : first;
}

export function relayAvatarTone(seed) {
  // Derive the tone so one Principal keeps the same color on every client.
  let hash = 2166136261;
  for (const character of String(seed || '')) {
    hash ^= character.codePointAt(0);
    hash = Math.imul(hash, 16777619);
  }
  const unsigned = hash >>> 0;
  const hue = (unsigned % 3600) / 10;
  const saturation = 62 + ((unsigned >>> 12) % 12);
  const backgroundLightness = 88 + ((unsigned >>> 20) % 5);
  const foregroundLightness = 27 + ((unsigned >>> 25) % 8);
  return {
    background: `hsl(${hue} ${saturation}% ${backgroundLightness}%)`,
    foreground: `hsl(${hue} ${Math.min(saturation + 8, 82)}% ${foregroundLightness}%)`,
  };
}
