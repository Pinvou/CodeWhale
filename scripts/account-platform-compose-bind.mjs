// Only for normalized `docker compose config` objects, never source YAML.
// compose-go's bool CreateHostPath uses json/yaml omitempty: false becomes an
// empty bind object, whereas short syntax and explicit true serialize as true.
// See compose-spec/compose-go v2.9.0 types/types.go and format/volume.go.
export function isReadOnlyDirectoryBind(volume) {
  const options = volume?.bind;
  return volume?.type === 'bind' && volume.read_only === true
    && options !== null && typeof options === 'object'
    && Object.getPrototypeOf(options) === Object.prototype
    && (!Object.hasOwn(options, 'create_host_path') || options.create_host_path === false);
}
