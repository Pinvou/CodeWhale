import React, { useEffect, useMemo, useRef, useState } from 'react';
import { useReducedMotion } from '../../hooks/useReducedMotion.js';
import {
  relayAvatarAssetSize,
  relayAvatarFallback,
  relayAvatarSourceRequest,
  relayAvatarTone,
} from './relay-avatar.mjs';

function browserPixelRatio() {
  return typeof window === 'undefined' ? 1 : window.devicePixelRatio || 1;
}

const avatarMeasurements = new Map();
let avatarResizeObserver = null;
let avatarMeasurementWindow = null;
const avatarReadiness = new Map();
let avatarIntersectionObserver = null;

function measureAllAvatars() {
  avatarMeasurements.forEach(measure => measure());
}

function waitForAvatarViewport(element, ready) {
  const browserWindow = typeof window === 'undefined' ? null : window;
  if (!browserWindow?.IntersectionObserver) {
    ready();
    return () => {};
  }
  avatarReadiness.set(element, ready);
  avatarIntersectionObserver ||= new browserWindow.IntersectionObserver(entries => {
    entries.forEach(entry => {
      if (!entry.isIntersecting) return;
      const markReady = avatarReadiness.get(entry.target);
      avatarReadiness.delete(entry.target);
      avatarIntersectionObserver?.unobserve(entry.target);
      markReady?.();
    });
  }, { rootMargin: '200px' });
  avatarIntersectionObserver.observe(element);
  return () => {
    avatarIntersectionObserver?.unobserve(element);
    avatarReadiness.delete(element);
    if (!avatarReadiness.size) {
      avatarIntersectionObserver?.disconnect();
      avatarIntersectionObserver = null;
    }
  };
}

function observeAvatarSize(element, measure) {
  const browserWindow = typeof window === 'undefined' ? null : window;
  measure();
  if (!browserWindow) return () => {};
  avatarMeasurements.set(element, measure);
  if (!avatarMeasurementWindow) {
    avatarMeasurementWindow = browserWindow;
    avatarMeasurementWindow.addEventListener('resize', measureAllAvatars);
  }
  if (browserWindow.ResizeObserver) {
    avatarResizeObserver ||= new browserWindow.ResizeObserver(entries => {
      entries.forEach(entry => avatarMeasurements.get(entry.target)?.());
    });
    avatarResizeObserver.observe(element);
  }
  return () => {
    avatarResizeObserver?.unobserve(element);
    avatarMeasurements.delete(element);
    if (!avatarMeasurements.size) {
      avatarResizeObserver?.disconnect();
      avatarResizeObserver = null;
      avatarMeasurementWindow?.removeEventListener('resize', measureAllAvatars);
      avatarMeasurementWindow = null;
    }
  };
}

export function RelayAvatar({ name, seed, avatar = null, loadSource = null, className = 'team-relay-avatar', displaySize, teamId = null }) {
  const rootRef = useRef(null);
  const useOriginal = displaySize === null;
  const reducedMotion = useReducedMotion();
  const [source, setSource] = useState(null);
  const [renderedPixels, setRenderedPixels] = useState(null);
  const [pixelRatio, setPixelRatio] = useState(browserPixelRatio);
  const [ready, setReady] = useState(() => {
    if (useOriginal) return true;
    return typeof window === 'undefined' || !window.IntersectionObserver;
  });
  const digest = avatar?.sha256 || '';
  const tone = useMemo(() => relayAvatarTone(seed || name), [name, seed]);
  const measuredDisplaySize = useMemo(() => {
    if (useOriginal) return null;
    if (displaySize === 48 || displaySize === 96) return displaySize;
    return renderedPixels == null ? undefined : relayAvatarAssetSize(renderedPixels, pixelRatio);
  }, [displaySize, pixelRatio, renderedPixels, useOriginal]);
  const sourceRequest = useMemo(() => relayAvatarSourceRequest(
    avatar?.content_type,
    measuredDisplaySize,
    reducedMotion,
  ), [avatar?.content_type, measuredDisplaySize, reducedMotion]);

  useEffect(() => {
    if (useOriginal || displaySize === 48 || displaySize === 96) return undefined;
    const element = rootRef.current;
    if (!element) return undefined;
    const measure = () => {
      const bounds = element.getBoundingClientRect();
      const pixels = Math.ceil(Math.max(bounds.width, bounds.height, element.offsetWidth, element.offsetHeight));
      if (pixels > 0) setRenderedPixels(current => current === pixels ? current : pixels);
      const nextPixelRatio = browserPixelRatio();
      setPixelRatio(current => current === nextPixelRatio ? current : nextPixelRatio);
    };
    return observeAvatarSize(element, measure);
  }, [displaySize, useOriginal]);

  useEffect(() => {
    if (ready || useOriginal) return undefined;
    const element = rootRef.current;
    if (!element) return undefined;
    return waitForAvatarViewport(element, () => setReady(true));
  }, [ready, useOriginal]);

  useEffect(() => {
    let active = true;
    setSource(null);
    if (typeof loadSource !== 'function') return () => { active = false; };
    if (!ready) return () => { active = false; };
    if (digest && sourceRequest.displaySize === undefined) return () => { active = false; };
    Promise.resolve()
      .then(() => active ? loadSource(
        seed || name,
        digest ? { sha256: digest } : null,
        sourceRequest.displaySize,
        sourceRequest.reducedMotion,
        teamId,
      ) : null)
      .then(value => { if (active) setSource(value || null); })
      .catch(() => { if (active) setSource(null); });
    return () => { active = false; };
  }, [digest, loadSource, name, ready, seed, sourceRequest, teamId]);

  return <span
    ref={rootRef}
    className={`${className} team-relay-user-avatar`.trim()}
    style={{
      '--relay-avatar-bg': tone.background,
      '--relay-avatar-fg': tone.foreground,
    }}
    aria-hidden="true"
  >
    {!source && <span>{relayAvatarFallback(name)}</span>}
    {source && <img src={source} alt="" loading="lazy" decoding="async" onError={() => setSource(null)} />}
  </span>;
}
