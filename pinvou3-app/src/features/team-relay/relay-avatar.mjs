export function relayAvatarFallback(name) {
  const characters = Array.from(String(name || '').trim());
  const first = characters.find(character => /[\p{L}\p{N}]/u.test(character));
  if (!first) return '?';
  return /^[a-z]$/i.test(first) ? first.toUpperCase() : first;
}

function hslRgb(hue, saturation, lightness) {
  const chroma = (1 - Math.abs((2 * lightness) - 1)) * saturation;
  const segment = (((hue % 360) + 360) % 360) / 60;
  const secondary = chroma * (1 - Math.abs((segment % 2) - 1));
  const [red, green, blue] = segment < 1 ? [chroma, secondary, 0]
    : segment < 2 ? [secondary, chroma, 0]
      : segment < 3 ? [0, chroma, secondary]
        : segment < 4 ? [0, secondary, chroma]
          : segment < 5 ? [secondary, 0, chroma]
            : [chroma, 0, secondary];
  const match = lightness - (chroma / 2);
  return [red + match, green + match, blue + match];
}

function relativeLuminance(rgb) {
  const linear = rgb.map(channel => channel <= 0.04045
    ? channel / 12.92
    : ((channel + 0.055) / 1.055) ** 2.4);
  return (0.2126 * linear[0]) + (0.7152 * linear[1]) + (0.0722 * linear[2]);
}

function colorRgb(value) {
  const hex = /^#([0-9a-f]{6})$/i.exec(String(value || '').trim());
  if (hex) {
    return [0, 2, 4].map(offset => Number.parseInt(hex[1].slice(offset, offset + 2), 16) / 255);
  }
  const hsl = /^hsl\(\s*([\d.]+)\s+([\d.]+)%\s+([\d.]+)%\s*\)$/i.exec(String(value || '').trim());
  if (!hsl) throw new TypeError(`Unsupported avatar color: ${value}`);
  return hslRgb(Number(hsl[1]), Number(hsl[2]) / 100, Number(hsl[3]) / 100);
}

export function relayAvatarContrastRatio(background, foreground) {
  const backgroundLuminance = relativeLuminance(colorRgb(background));
  const foregroundLuminance = relativeLuminance(colorRgb(foreground));
  const lighter = Math.max(backgroundLuminance, foregroundLuminance);
  const darker = Math.min(backgroundLuminance, foregroundLuminance);
  return (lighter + 0.05) / (darker + 0.05);
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
  const saturation = 58 + ((unsigned >>> 12) % 18);
  const backgroundLightness = 38 + ((unsigned >>> 20) % 45);
  const background = `hsl(${hue} ${saturation}% ${backgroundLightness}%)`;
  const foreground = relayAvatarContrastRatio(background, '#000000')
    >= relayAvatarContrastRatio(background, '#ffffff')
    ? '#000000'
    : '#ffffff';
  return {
    background,
    foreground,
  };
}

export function relayAvatarAssetSize(renderedPixels, pixelRatio = 1) {
  const logicalPixels = Math.max(1, Number(renderedPixels) || 1);
  return logicalPixels * Math.max(1, Number(pixelRatio) || 1) <= 48 ? 48 : 96;
}
