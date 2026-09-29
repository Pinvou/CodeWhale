import { open } from "node:fs/promises";
import { badRequest } from "./errors.js";

const IMAGE_FILE_MACHINE_I386 = 0x014c;
const IMAGE_FILE_MACHINE_AMD64 = 0x8664;
const IMAGE_FILE_EXECUTABLE_IMAGE = 0x0002;
const IMAGE_SCN_MEM_EXECUTE = 0x20000000;
const PE32_MAGIC = 0x10b;
const PE32_PLUS_MAGIC = 0x20b;
const RT_VERSION = 16;
const VS_FIXEDFILEINFO_SIGNATURE = 0xfeef04bd;
const MAX_RESOURCE_BYTES = 16 * 1024 * 1024;

function invalid(message) {
  return badRequest("invalid_installer", message);
}

function checkedRange(buffer, offset, length, label) {
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(length)
      || offset < 0 || length < 0 || offset + length > buffer.length) {
    throw invalid(`安装器的 ${label} 结构越界`);
  }
}

function align4(value) {
  return (value + 3) & ~3;
}

async function readExact(handle, offset, length, fileSize, label) {
  if (!Number.isSafeInteger(offset) || offset < 0 || length < 0 || offset + length > fileSize) {
    throw invalid(`安装器的 ${label} 结构越界`);
  }
  const buffer = Buffer.alloc(length);
  const { bytesRead } = await handle.read(buffer, 0, length, offset);
  if (bytesRead !== length) throw invalid(`安装器的 ${label} 结构不完整`);
  return buffer;
}

function powerOfTwo(value) {
  return value > 0 && (value & (value - 1)) === 0;
}

function rvaToFileOffset(rva, length, sections, fileSize) {
  for (const section of sections) {
    const mappedSize = Math.max(section.virtualSize, section.rawSize);
    if (rva < section.virtualAddress || rva + length > section.virtualAddress + mappedSize) continue;
    const delta = rva - section.virtualAddress;
    if (delta + length > section.rawSize) break;
    const offset = section.rawPointer + delta;
    if (offset + length <= fileSize) return offset;
    break;
  }
  throw invalid("安装器的 RVA 无法映射到文件内容");
}

function resourceEntries(resource, directoryOffset) {
  checkedRange(resource, directoryOffset, 16, "资源目录");
  const namedCount = resource.readUInt16LE(directoryOffset + 12);
  const idCount = resource.readUInt16LE(directoryOffset + 14);
  const count = namedCount + idCount;
  if (count < 1 || count > 4096) throw invalid("安装器的资源目录条目数无效");
  checkedRange(resource, directoryOffset + 16, count * 8, "资源目录条目");
  return Array.from({ length: count }, (_, index) => {
    const offset = directoryOffset + 16 + index * 8;
    return {
      name: resource.readUInt32LE(offset),
      target: resource.readUInt32LE(offset + 4),
    };
  });
}

function versionDataEntry(resource) {
  const rootEntry = resourceEntries(resource, 0).find(
    (entry) => (entry.name & 0x80000000) === 0 && entry.name === RT_VERSION,
  );
  if (!rootEntry || (rootEntry.target & 0x80000000) === 0) {
    throw invalid("安装器缺少 VERSIONINFO 资源");
  }
  let target = rootEntry.target;
  for (let depth = 0; depth < 4; depth += 1) {
    if ((target & 0x80000000) === 0) {
      const dataOffset = target;
      checkedRange(resource, dataOffset, 16, "VERSIONINFO 数据项");
      return {
        rva: resource.readUInt32LE(dataOffset),
        size: resource.readUInt32LE(dataOffset + 4),
      };
    }
    const directoryOffset = target & 0x7fffffff;
    const entries = resourceEntries(resource, directoryOffset);
    target = entries[0].target;
  }
  throw invalid("安装器的 VERSIONINFO 资源层级无效");
}

function readUtf16Key(buffer, offset) {
  for (let cursor = offset; cursor + 1 < buffer.length; cursor += 2) {
    if (buffer.readUInt16LE(cursor) === 0) {
      return { value: buffer.toString("utf16le", offset, cursor), end: cursor + 2 };
    }
  }
  throw invalid("安装器的 VERSIONINFO 字符串未终止");
}

function fixedVersion(ms, ls) {
  return [ms >>> 16, ms & 0xffff, ls >>> 16, ls & 0xffff].join(".");
}

function versionString(buffer, key) {
  const encoded = Buffer.from(`${key}\0`, "utf16le");
  let keyOffset = buffer.indexOf(encoded);
  while (keyOffset >= 6) {
    const blockOffset = keyOffset - 6;
    const length = buffer.readUInt16LE(blockOffset);
    const valueLength = buffer.readUInt16LE(blockOffset + 2);
    const type = buffer.readUInt16LE(blockOffset + 4);
    if (type === 1 && length >= 6 + encoded.length && blockOffset + length <= buffer.length) {
      const valueOffset = align4(keyOffset + encoded.length);
      const valueBytes = valueLength * 2;
      if (valueLength > 0 && valueOffset + valueBytes <= blockOffset + length) {
        return buffer.toString("utf16le", valueOffset, valueOffset + valueBytes).replace(/\0+$/u, "").trim();
      }
    }
    keyOffset = buffer.indexOf(encoded, keyOffset + 2);
  }
  return null;
}

function normalizeVersionString(value) {
  if (typeof value !== "string") return null;
  const match = /^\s*(\d+)\s*[.,]\s*(\d+)\s*[.,]\s*(\d+)(?:\s*[.,]\s*(\d+))?\s*$/u.exec(value);
  if (!match) return null;
  return match.slice(1).filter((part) => part !== undefined).map(Number);
}

function versionsEqual(actual, expected) {
  const left = normalizeVersionString(actual);
  const right = normalizeVersionString(expected);
  if (!left || !right) return false;
  const width = Math.max(left.length, right.length);
  return Array.from({ length: width }).every((_, index) => (left[index] ?? 0) === (right[index] ?? 0));
}

function parseVersionInfo(buffer) {
  checkedRange(buffer, 0, 6, "VERSIONINFO");
  const totalLength = buffer.readUInt16LE(0);
  const valueLength = buffer.readUInt16LE(2);
  if (totalLength < 6 || totalLength > buffer.length || valueLength < 52) {
    throw invalid("安装器的 VERSIONINFO 长度无效");
  }
  const key = readUtf16Key(buffer, 6);
  if (key.value !== "VS_VERSION_INFO") throw invalid("安装器的 VERSIONINFO 根键无效");
  const fixedOffset = align4(key.end);
  checkedRange(buffer, fixedOffset, 52, "VS_FIXEDFILEINFO");
  if (buffer.readUInt32LE(fixedOffset) !== VS_FIXEDFILEINFO_SIGNATURE) {
    throw invalid("安装器的 VS_FIXEDFILEINFO 签名无效");
  }
  const fixedFile = fixedVersion(
    buffer.readUInt32LE(fixedOffset + 8),
    buffer.readUInt32LE(fixedOffset + 12),
  );
  const fixedProduct = fixedVersion(
    buffer.readUInt32LE(fixedOffset + 16),
    buffer.readUInt32LE(fixedOffset + 20),
  );
  const productString = versionString(buffer.subarray(0, totalLength), "ProductVersion");
  const fileString = versionString(buffer.subarray(0, totalLength), "FileVersion");
  if (productString) return { version: productString, source: "ProductVersion" };
  if (fixedProduct) return { version: fixedProduct, source: "VS_FIXEDFILEINFO.ProductVersion" };
  if (fileString) return { version: fileString, source: "FileVersion" };
  return { version: fixedFile, source: "VS_FIXEDFILEINFO.FileVersion" };
}

export async function inspectPeInstaller(path, expectedVersion) {
  const handle = await open(path, "r");
  try {
    const file = await handle.stat();
    if (file.size < 512) throw invalid("安装器文件过小，无法构成有效 PE 映像");
    const dosHeader = await readExact(handle, 0, 64, file.size, "DOS 头");
    if (dosHeader.toString("ascii", 0, 2) !== "MZ") throw invalid("安装器缺少 DOS MZ 签名");
    const peOffset = dosHeader.readUInt32LE(0x3c);
    const peHeader = await readExact(handle, peOffset, 24, file.size, "PE/COFF 头");
    if (!peHeader.subarray(0, 4).equals(Buffer.from([0x50, 0x45, 0, 0]))) {
      throw invalid("安装器的 PE 签名无效");
    }
    const machine = peHeader.readUInt16LE(4);
    const sectionCount = peHeader.readUInt16LE(6);
    const optionalSize = peHeader.readUInt16LE(20);
    const characteristics = peHeader.readUInt16LE(22);
    if (sectionCount < 1 || sectionCount > 96 || optionalSize < 120
        || (characteristics & IMAGE_FILE_EXECUTABLE_IMAGE) === 0) {
      throw invalid("安装器的 COFF 可执行映像结构无效");
    }
    const optionalOffset = peOffset + 24;
    const optional = await readExact(handle, optionalOffset, optionalSize, file.size, "可选头");
    const magic = optional.readUInt16LE(0);
    const supportedStub = (machine === IMAGE_FILE_MACHINE_I386 && magic === PE32_MAGIC)
      || (machine === IMAGE_FILE_MACHINE_AMD64 && magic === PE32_PLUS_MAGIC);
    if (!supportedStub) {
      throw badRequest(
        "invalid_installer_arch",
        "NSIS 启动外壳必须是 I386/PE32 或 AMD64/PE32+ 可执行映像",
      );
    }
    const directoryCountOffset = magic === PE32_MAGIC ? 92 : 108;
    const resourceDirectoryOffset = magic === PE32_MAGIC ? 112 : 128;
    if (optionalSize < resourceDirectoryOffset + 8) {
      throw invalid("安装器的可选头缺少资源目录");
    }
    const entryPoint = optional.readUInt32LE(16);
    const sectionAlignment = optional.readUInt32LE(32);
    const fileAlignment = optional.readUInt32LE(36);
    const sizeOfImage = optional.readUInt32LE(56);
    const sizeOfHeaders = optional.readUInt32LE(60);
    const directoryCount = optional.readUInt32LE(directoryCountOffset);
    if (!entryPoint || !powerOfTwo(fileAlignment) || fileAlignment < 512 || fileAlignment > 65536
        || !powerOfTwo(sectionAlignment) || sectionAlignment < fileAlignment
        || sizeOfImage <= sizeOfHeaders || sizeOfHeaders > file.size || directoryCount < 3) {
      throw invalid("安装器的 PE 映像布局无效");
    }
    const sectionTableOffset = optionalOffset + optionalSize;
    if (sectionTableOffset + sectionCount * 40 > sizeOfHeaders) {
      throw invalid("安装器的节表超出 PE 头范围");
    }
    const sectionTable = await readExact(
      handle,
      sectionTableOffset,
      sectionCount * 40,
      file.size,
      "节表",
    );
    const sections = [];
    for (let index = 0; index < sectionCount; index += 1) {
      const offset = index * 40;
      const section = {
        virtualSize: sectionTable.readUInt32LE(offset + 8),
        virtualAddress: sectionTable.readUInt32LE(offset + 12),
        rawSize: sectionTable.readUInt32LE(offset + 16),
        rawPointer: sectionTable.readUInt32LE(offset + 20),
        characteristics: sectionTable.readUInt32LE(offset + 36),
      };
      if (!section.virtualAddress || section.rawPointer + section.rawSize > file.size) {
        throw invalid("安装器的节范围无效");
      }
      sections.push(section);
    }
    const entrySection = sections.find((section) => (
      entryPoint >= section.virtualAddress
      && entryPoint < section.virtualAddress + Math.max(section.virtualSize, section.rawSize)
    ));
    if (!entrySection || (entrySection.characteristics & IMAGE_SCN_MEM_EXECUTE) === 0) {
      throw invalid("安装器入口点不在可执行节内");
    }

    const resourceRva = optional.readUInt32LE(resourceDirectoryOffset);
    const resourceSize = optional.readUInt32LE(resourceDirectoryOffset + 4);
    if (!resourceRva || resourceSize < 24 || resourceSize > MAX_RESOURCE_BYTES) {
      throw invalid("安装器缺少合理的 PE 资源目录");
    }
    const resourceOffset = rvaToFileOffset(resourceRva, resourceSize, sections, file.size);
    const resource = await readExact(handle, resourceOffset, resourceSize, file.size, "资源目录");
    const versionEntry = versionDataEntry(resource);
    if (!versionEntry.size || versionEntry.size > 1024 * 1024) {
      throw invalid("安装器的 VERSIONINFO 大小无效");
    }
    const versionOffset = rvaToFileOffset(versionEntry.rva, versionEntry.size, sections, file.size);
    const versionBuffer = await readExact(handle, versionOffset, versionEntry.size, file.size, "VERSIONINFO");
    const version = parseVersionInfo(versionBuffer);
    if (!versionsEqual(version.version, expectedVersion)) {
      throw badRequest(
        "installer_version_mismatch",
        `安装器 ${version.source}=${version.version} 与发布版本 ${expectedVersion} 不一致`,
      );
    }
    return {
      machine: machine === IMAGE_FILE_MACHINE_I386 ? "I386" : "AMD64",
      imageFormat: magic === PE32_MAGIC ? "PE32" : "PE32+",
      version: version.version,
      versionSource: version.source,
    };
  } finally {
    await handle.close();
  }
}
