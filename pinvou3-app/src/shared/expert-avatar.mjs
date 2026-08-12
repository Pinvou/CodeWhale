export const EXPERT_AVATAR_COUNT = 50;

export function expertAvatarSrc(seed) {
  let hash = 0;
  const text = String(seed || '');
  for (let index = 0; index < text.length; index += 1) {
    hash = (hash * 31 + text.charCodeAt(index)) >>> 0;
  }
  const number = (hash % EXPERT_AVATAR_COUNT) + 1;
  return `avatars/avatar-${String(number).padStart(2, '0')}.svg`;
}
