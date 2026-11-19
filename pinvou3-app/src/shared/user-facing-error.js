// 用户可见错误码助手：从后端对象/消息前缀解析 snake_case 错误码，并把错误码
// 映射到调用方提供的本地化文案，永不把原始后端诊断直接交给界面。
// community #860 回流加固：查表前先做自有键与字符串类型守卫（对齐
// connectorErrorCopy 的 house pattern）——'constructor' 能通过 snake_case
// 校验，裸查表会解析到继承的 Object 构造函数；i18n 词典还含函数值/数组值键，
// 命中时必须回落到 fallback 而不是把非字符串值交给 DOM。
const ERROR_CODE_PATTERN = /^[a-z][a-z0-9_]*$/;

function errorCode(value) {
  if (value && typeof value === 'object' && typeof value.code === 'string') {
    const code = value.code.trim();
    if (ERROR_CODE_PATTERN.test(code)) return code;
  }

  const message = typeof value === 'string'
    ? value
    : (value && typeof value.message === 'string' ? value.message : '');
  const separator = message.indexOf(':');
  if (separator <= 0) return '';
  const code = message.slice(0, separator).trim();
  return ERROR_CODE_PATTERN.test(code) ? code : '';
}

function localizedErrorMessage(value, messages, fallback) {
  const code = errorCode(value);
  // Mirror the full `connectorErrorCopy` house pattern: guard the prototype
  // chain before the dict lookup ('constructor' passes the snake_case pattern
  // and would otherwise resolve to the inherited Object constructor), and
  // only ever return string copy — i18n dicts also hold function- and
  // array-valued keys that must fall through instead of reaching the DOM.
  if (
    !code || !messages
    || !Object.prototype.hasOwnProperty.call(messages, code)
    || typeof messages[code] !== 'string'
  ) {
    return fallback;
  }
  return messages[code] || fallback;
}

export { errorCode, localizedErrorMessage };
