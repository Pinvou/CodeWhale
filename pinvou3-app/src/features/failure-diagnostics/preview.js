export function isDiagnosticPreview(value) {
  return typeof value?.preview_token === 'string' && value.preview_token.length > 0
    && value.report !== null && typeof value.report === 'object' && !Array.isArray(value.report)
    && Array.isArray(value.report.attachments) && value.report.attachments.length <= 3
    && value.report.attachments.every(item => item !== null && typeof item === 'object'
      && !Array.isArray(item) && ['text', 'image/png', 'image/jpeg'].includes(item.kind)
      && typeof item.content === 'string');
}
