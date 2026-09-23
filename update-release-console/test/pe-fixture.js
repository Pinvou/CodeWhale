function align4(value) {
  return (value + 3) & ~3;
}

function utf16(value) {
  return Buffer.from(`${value}\0`, "utf16le");
}

function versionParts(version) {
  const parts = version.split(".").map(Number);
  while (parts.length < 4) parts.push(0);
  return parts;
}

function versionDwords(version) {
  const parts = versionParts(version);
  return [(parts[0] << 16) | parts[1], (parts[2] << 16) | parts[3]];
}

function versionStringBlock(key, value) {
  const keyBytes = utf16(key);
  const valueBytes = utf16(value);
  const valueOffset = align4(6 + keyBytes.length);
  const length = valueOffset + valueBytes.length;
  const block = Buffer.alloc(align4(length));
  block.writeUInt16LE(length, 0);
  block.writeUInt16LE(value.length + 1, 2);
  block.writeUInt16LE(1, 4);
  keyBytes.copy(block, 6);
  valueBytes.copy(block, valueOffset);
  return block;
}

function containerBlock(key, children, type = 1) {
  const keyBytes = utf16(key);
  const childOffset = align4(6 + keyBytes.length);
  const length = childOffset + children.reduce((size, child) => size + child.length, 0);
  const block = Buffer.alloc(align4(length));
  block.writeUInt16LE(length, 0);
  block.writeUInt16LE(0, 2);
  block.writeUInt16LE(type, 4);
  keyBytes.copy(block, 6);
  let offset = childOffset;
  for (const child of children) {
    child.copy(block, offset);
    offset += child.length;
  }
  return block;
}

function versionInfo(productVersion, fileVersion = productVersion) {
  const product = versionStringBlock("ProductVersion", productVersion);
  const file = versionStringBlock("FileVersion", fileVersion);
  const table = containerBlock("040904B0", [product, file]);
  const stringFileInfo = containerBlock("StringFileInfo", [table]);
  const key = utf16("VS_VERSION_INFO");
  const fixedOffset = align4(6 + key.length);
  const fixed = Buffer.alloc(52);
  const [fileMs, fileLs] = versionDwords(fileVersion);
  const [productMs, productLs] = versionDwords(productVersion);
  fixed.writeUInt32LE(0xfeef04bd, 0);
  fixed.writeUInt32LE(0x00010000, 4);
  fixed.writeUInt32LE(fileMs >>> 0, 8);
  fixed.writeUInt32LE(fileLs >>> 0, 12);
  fixed.writeUInt32LE(productMs >>> 0, 16);
  fixed.writeUInt32LE(productLs >>> 0, 20);
  fixed.writeUInt32LE(0x3f, 24);
  fixed.writeUInt32LE(0, 28);
  fixed.writeUInt32LE(0x00040004, 32);
  fixed.writeUInt32LE(1, 36);
  const childOffset = align4(fixedOffset + fixed.length);
  const length = childOffset + stringFileInfo.length;
  const root = Buffer.alloc(align4(length));
  root.writeUInt16LE(length, 0);
  root.writeUInt16LE(fixed.length, 2);
  root.writeUInt16LE(0, 4);
  key.copy(root, 6);
  fixed.copy(root, fixedOffset);
  stringFileInfo.copy(root, childOffset);
  return root;
}

function sectionHeader(name, virtualSize, virtualAddress, rawSize, rawPointer, characteristics) {
  const header = Buffer.alloc(40);
  header.write(name, 0, 8, "ascii");
  header.writeUInt32LE(virtualSize, 8);
  header.writeUInt32LE(virtualAddress, 12);
  header.writeUInt32LE(rawSize, 16);
  header.writeUInt32LE(rawPointer, 20);
  header.writeUInt32LE(characteristics >>> 0, 36);
  return header;
}

export function peFixture({
  productVersion = "0.10.4.0",
  fileVersion = productVersion,
  machine = 0x014c,
  imageFormat = "pe32",
} = {}) {
  const peOffset = 0x80;
  const headersSize = 0x200;
  const textRaw = 0x200;
  const resourceRaw = 0x400;
  const resourceRawSize = 0x800;
  const textRva = 0x1000;
  const resourceRva = 0x2000;
  const content = Buffer.alloc(resourceRaw + resourceRawSize);
  content.write("MZ", 0, "ascii");
  content.writeUInt32LE(peOffset, 0x3c);
  content.write("PE\0\0", peOffset, "binary");
  const coff = peOffset + 4;
  content.writeUInt16LE(machine, coff);
  content.writeUInt16LE(2, coff + 2);
  const pe32Plus = imageFormat === "pe32+";
  const optionalSize = pe32Plus ? 0xf0 : 0xe0;
  content.writeUInt16LE(optionalSize, coff + 16);
  content.writeUInt16LE(0x0022, coff + 18);
  const optional = coff + 20;
  content.writeUInt16LE(pe32Plus ? 0x20b : 0x10b, optional);
  content.writeUInt32LE(textRva, optional + 16);
  if (pe32Plus) content.writeBigUInt64LE(0x140000000n, optional + 24);
  else content.writeUInt32LE(0x400000, optional + 28);
  content.writeUInt32LE(0x1000, optional + 32);
  content.writeUInt32LE(0x200, optional + 36);
  content.writeUInt32LE(0x3000, optional + 56);
  content.writeUInt32LE(headersSize, optional + 60);
  content.writeUInt16LE(3, optional + 68);
  const directoryCountOffset = pe32Plus ? 108 : 92;
  const resourceDirectoryOffset = pe32Plus ? 128 : 112;
  content.writeUInt32LE(16, optional + directoryCountOffset);
  content.writeUInt32LE(resourceRva, optional + resourceDirectoryOffset);
  content.writeUInt32LE(resourceRawSize, optional + resourceDirectoryOffset + 4);
  const sectionTable = optional + optionalSize;
  sectionHeader(".text", 1, textRva, 0x200, textRaw, 0x60000020).copy(content, sectionTable);
  sectionHeader(".rsrc", resourceRawSize, resourceRva, resourceRawSize, resourceRaw, 0x40000040)
    .copy(content, sectionTable + 40);
  content[textRaw] = 0xc3;

  const resource = content.subarray(resourceRaw, resourceRaw + resourceRawSize);
  resource.writeUInt16LE(1, 14);
  resource.writeUInt32LE(16, 16);
  resource.writeUInt32LE(0x80000018, 20);
  resource.writeUInt16LE(1, 24 + 14);
  resource.writeUInt32LE(1, 24 + 16);
  resource.writeUInt32LE(0x80000030, 24 + 20);
  resource.writeUInt16LE(1, 48 + 14);
  resource.writeUInt32LE(0x0409, 48 + 16);
  resource.writeUInt32LE(72, 48 + 20);
  const version = versionInfo(productVersion, fileVersion);
  resource.writeUInt32LE(resourceRva + 0x100, 72);
  resource.writeUInt32LE(version.length, 76);
  resource.writeUInt32LE(1200, 80);
  version.copy(resource, 0x100);
  return content;
}
