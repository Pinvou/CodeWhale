#!/usr/bin/env node
import { readFile, stat } from "node:fs/promises";
import { isDirectCliEntry } from "../cli-entrypoint.js";
import { renderEnrollmentNginxConfig } from "../enrollment-nginx-config.js";

export async function main(args = process.argv.slice(2), env = process.env, io = console) {
  try {
    if (args.length !== 0 && !(args.length === 2 && args[0] === "--check")) {
      throw Object.assign(new Error(), { code: "gateway_nginx_arguments_invalid" });
    }
    const generated = renderEnrollmentNginxConfig(env);
    if (args.length === 0) {
      io.log(generated.trimEnd());
    } else {
      const info = await stat(args[1]);
      if (!info.isFile() || info.size > 64 * 1024) {
        throw Object.assign(new Error(), { code: "gateway_nginx_config_invalid" });
      }
      const contents = await readFile(args[1], "utf8");
      if (contents.replaceAll("\r\n", "\n") !== generated) {
        throw Object.assign(new Error(), { code: "gateway_nginx_config_drift" });
      }
      io.log(JSON.stringify({ ok: true, gateway_nginx_config: "consistent" }));
    }
    return 0;
  } catch (error) {
    const known = typeof error?.code === "string" && error.code.startsWith("gateway_");
    io.error(JSON.stringify({ ok: false, code: known ? error.code : "gateway_nginx_config_unavailable" }));
    return 1;
  }
}

if (isDirectCliEntry(import.meta.url)) process.exitCode = await main();
