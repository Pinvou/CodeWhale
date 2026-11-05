// Only for normalized `docker compose config` objects, never source YAML.
// An omitted create_host_path is ambiguous: compose-go v2.9.0 omits false,
// whereas v2.14.0's OptOut omits true, including short-syntax binds.
// Require explicit false instead of inferring safety from a renderer default.
export function isReadOnlyDirectoryBind(volume) {
  const options = volume?.bind;
  return volume?.type === 'bind' && volume.read_only === true
    && options !== null && typeof options === 'object'
    && Object.getPrototypeOf(options) === Object.prototype
    && Object.hasOwn(options, 'create_host_path') && options.create_host_path === false;
}
