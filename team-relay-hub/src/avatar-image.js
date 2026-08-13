import { createHash } from "node:crypto";
import { extname } from "node:path";
import { fileTypeFromFile } from "file-type";
import sharp from "sharp";
import { AppError } from "./errors.js";

export const MAX_AVATAR_BYTES = 1_572_864;
export const MAX_STATIC_EDGE = 512;
export const MAX_GIF_EDGE = 384;
export const MAX_GIF_FRAMES = 60;
export const MAX_GIF_PIXELS = 4_000_000;
export const MAX_STATIC_SOURCE_PIXELS = 8_000_000;
export const AVATAR_VARIANT_SIZES = Object.freeze([48, 96]);

const ALLOWED_AVATAR_EXTENSIONS = new Map([
  ["png", "png"],
  ["jpg", "jpg"],
  ["jpeg", "jpg"],
  ["webp", "webp"],
  ["gif", "gif"],
]);
const FORMAT_CONTENT_TYPES = new Map([
  ["png", "image/png"],
  ["jpeg", "image/jpeg"],
  ["webp", "image/webp"],
  ["gif", "image/gif"],
]);

// Avatar processing is deliberately bounded outside libvips too. A small cache
// helps repeated variant generation without retaining arbitrary user images.
sharp.cache({ memory: 16, files: 0, items: 32 });

function invalidImage(message) {
  return new AppError(422, "avatar_image_invalid", message);
}

function limitExceeded(message) {
  return new AppError(422, "avatar_image_limits_exceeded", message);
}

function positiveInteger(value) {
  return Number.isSafeInteger(value) && value > 0;
}

export async function detectAvatarContent(upload) {
  const expected = ALLOWED_AVATAR_EXTENSIONS.get(
    extname(upload.filename).slice(1).toLowerCase(),
  );
  if (!expected) {
    throw new AppError(415, "avatar_type_not_allowed", "The avatar file type is not allowed.");
  }
  let detected;
  try {
    detected = await fileTypeFromFile(upload.temporaryPath);
  } catch {
    detected = undefined;
  }
  if (!detected || detected.ext !== expected || !FORMAT_CONTENT_TYPES.has(detected.ext === "jpg" ? "jpeg" : detected.ext)) {
    throw new AppError(415, "avatar_type_not_allowed", "The avatar content does not match its extension.");
  }
  return detected.mime;
}

export function validateAvatarMetadata(metadata, expectedContentType) {
  const contentType = FORMAT_CONTENT_TYPES.get(metadata?.format);
  if (!contentType || contentType !== expectedContentType) {
    throw invalidImage("The avatar could not be decoded as the declared image type.");
  }

  const frames = metadata.pages ?? 1;
  const width = metadata.width;
  const frameHeight = metadata.pageHeight ?? metadata.height;
  if (!positiveInteger(width) || !positiveInteger(frameHeight) || !positiveInteger(frames)) {
    throw invalidImage("The avatar has invalid image dimensions.");
  }

  if (metadata.format !== "gif" && frames !== 1) {
    throw new AppError(
      415,
      "avatar_type_not_allowed",
      "Only GIF files may contain animation.",
    );
  }

  const decodedPixels = width * frameHeight * frames;
  if (!Number.isSafeInteger(decodedPixels)) {
    throw limitExceeded("The avatar decoded pixel count is too large.");
  }
  if (metadata.format === "gif") {
    if (frames > MAX_GIF_FRAMES) {
      throw limitExceeded(`Animated avatars cannot exceed ${MAX_GIF_FRAMES} frames.`);
    }
    if (decodedPixels > MAX_GIF_PIXELS) {
      throw limitExceeded(`Animated avatars cannot exceed ${MAX_GIF_PIXELS} decoded pixels.`);
    }
  } else if (decodedPixels > MAX_STATIC_SOURCE_PIXELS) {
    throw limitExceeded(`Static avatar sources cannot exceed ${MAX_STATIC_SOURCE_PIXELS} pixels.`);
  }

  return {
    format: metadata.format,
    contentType,
    width,
    height: frameHeight,
    frames,
    decodedPixels,
    edgeLimit: metadata.format === "gif" ? MAX_GIF_EDGE : MAX_STATIC_EDGE,
    delay: metadata.format === "gif" ? metadata.delay : undefined,
    loop: metadata.format === "gif" ? metadata.loop : undefined,
  };
}

function transform(input, descriptor, edge) {
  return sharp(input, {
    animated: descriptor.format === "gif",
    failOn: "error",
    limitInputPixels: MAX_STATIC_SOURCE_PIXELS,
    sequentialRead: true,
  })
    .rotate()
    .resize({
      width: edge,
      height: edge,
      fit: "inside",
      withoutEnlargement: true,
    });
}

async function encodeAttempt(input, descriptor, edge, setting) {
  const image = transform(input, descriptor, edge);
  switch (descriptor.format) {
    case "gif":
      return image.gif({
        effort: 7,
        colours: setting,
        dither: 0.8,
        // Relay avatars stay animated while visible, including source GIFs
        // that were exported with a finite loop count.
        loop: 0,
        delay: descriptor.delay,
      }).toBuffer();
    case "jpeg":
      return image.jpeg({
        quality: setting,
        mozjpeg: true,
        chromaSubsampling: "4:4:4",
      }).toBuffer();
    case "webp":
      return image.webp({ quality: setting, effort: 5, smartSubsample: true }).toBuffer();
    case "png":
      return image.png({ compressionLevel: 9, adaptiveFiltering: true }).toBuffer();
    default:
      throw invalidImage("The avatar image type is unsupported.");
  }
}

async function encodeBounded(input, descriptor, edge, maximumBytes, variant = false) {
  const settings = descriptor.format === "gif"
    ? (variant ? [128, 96, 64] : [256, 192, 128, 96, 64])
    : descriptor.format === "png" ? [undefined] : [88, 82, 76, 70];
  let data;
  for (const setting of settings) {
    try {
      data = await encodeAttempt(input, descriptor, edge, setting);
    } catch {
      throw invalidImage("The avatar is damaged or could not be fully decoded.");
    }
    if (data.length <= maximumBytes) return data;
  }
  throw new AppError(
    413,
    "avatar_processed_too_large",
    "The processed avatar exceeds the size limit.",
  );
}

async function outputAsset(data, expectedDescriptor, maximumBytes) {
  let metadata;
  try {
    metadata = await sharp(data, {
      animated: expectedDescriptor.format === "gif",
      failOn: "error",
      limitInputPixels: MAX_STATIC_SOURCE_PIXELS,
    }).metadata();
  } catch {
    throw invalidImage("The processed avatar could not be verified.");
  }
  const descriptor = validateAvatarMetadata(metadata, expectedDescriptor.contentType);
  if (data.length === 0 || data.length > maximumBytes) {
    throw new AppError(413, "avatar_processed_too_large", "The processed avatar exceeds the size limit.");
  }
  return {
    data,
    contentType: descriptor.contentType,
    byteSize: data.length,
    sha256: createHash("sha256").update(data).digest("hex"),
    width: descriptor.width,
    height: descriptor.height,
    frames: descriptor.frames,
  };
}

export async function processAvatarImage(upload, config) {
  const maximumBytes = Math.min(config.maxAvatarBytes, MAX_AVATAR_BYTES);
  if (upload.byteSize > maximumBytes) {
    throw new AppError(413, "avatar_too_large", "The avatar exceeds the size limit.");
  }
  const expectedContentType = await detectAvatarContent(upload);
  let metadata;
  try {
    metadata = await sharp(upload.temporaryPath, {
      animated: true,
      failOn: "error",
      // Metadata inspection does not allocate the decoded bitmap. Read the
      // declared dimensions first so policy violations receive the precise
      // limits error; every subsequent full decode still enforces libvips'
      // pixel cap in transform().
      limitInputPixels: false,
      sequentialRead: true,
    }).metadata();
  } catch {
    throw invalidImage("The avatar is damaged or exceeds the safe decode limit.");
  }
  const descriptor = validateAvatarMetadata(metadata, expectedContentType);

  // Re-encoding is the security boundary: it fully decodes the source, applies
  // orientation, strips metadata, and emits only formats controlled by the Hub.
  const mainData = await encodeBounded(
    upload.temporaryPath,
    descriptor,
    descriptor.edgeLimit,
    maximumBytes,
  );
  const main = await outputAsset(mainData, descriptor, maximumBytes);

  const variants = new Map();
  for (const size of AVATAR_VARIANT_SIZES) {
    const data = await encodeBounded(mainData, descriptor, size, maximumBytes, true);
    variants.set(size, await outputAsset(data, descriptor, maximumBytes));
  }
  return { main, variants };
}
