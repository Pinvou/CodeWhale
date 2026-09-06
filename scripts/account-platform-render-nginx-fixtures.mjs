#!/usr/bin/env node
// Public CI fixtures only: no certificate issuance, network, reload or site configuration.
import { mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { renderCredentialEdge, credentialOriginPairs } from './account-platform-credential-edge.mjs';

export async function renderNginxFixtures(outputDirectory, runtimeDirectory) {
  if (!path.isAbsolute(outputDirectory) || typeof runtimeDirectory !== 'string'
    || !/^\/(?:[A-Za-z0-9_-]+\/)*[A-Za-z0-9_-]+$/.test(runtimeDirectory)) throw new Error('NGINX_FIXTURE_PATH_INVALID');
  await mkdir(outputDirectory, { recursive: true });
  if ((await readdir(outputDirectory)).length !== 0) throw new Error('NGINX_FIXTURE_DIRECTORY_NOT_EMPTY');
  const baseline = JSON.parse(await readFile(new URL('../account-platform/deploy/nginx/credential-edge.example.json', import.meta.url), 'utf8'));
  const files = [];
  for (const [environment, pair] of Object.entries(credentialOriginPairs)) {
    for (const transport of ['loopback', 'encrypted_tunnel', 'verified_tls']) {
      const config = structuredClone(baseline);
      Object.assign(config, { environment, portal_origin: pair.portal, credential_origin: pair.credential });
      config.tls = { certificate_file: `${runtimeDirectory}/fixture.crt`, private_key_file: `${runtimeDirectory}/fixture.key`, access_log_file: `${runtimeDirectory}/safe.log` };
      for (const name of ['portal', 'credential']) {
        if (transport === 'encrypted_tunnel') config.upstreams[name].mode = transport;
        if (transport === 'verified_tls') config.upstreams[name] = { url: 'https://localhost', mode: transport, ca_file: `${runtimeDirectory}/fixture.crt`, security_reference: 'isolated-parser-fixture' };
      }
      const [http, credential, handoff] = await Promise.all(['http', 'credential', 'handoff'].map(section => renderCredentialEdge(config, section)));
      const filename = `${environment.toLowerCase()}-${transport}.conf`;
      const temporaryPaths = ['client_body', 'proxy', 'fastcgi', 'uwsgi', 'scgi'].map(kind => `${kind}_temp_path ${runtimeDirectory}/${kind}-temp;`).join('\n');
      const result = `pid ${runtimeDirectory}/fixture.pid;\nerror_log ${runtimeDirectory}/parser.log notice;\nevents {}\nhttp {\n${temporaryPaths}\n${http}\n${credential}\nserver { listen 443 ssl; server_name ${new URL(pair.portal).hostname}; ssl_certificate ${runtimeDirectory}/fixture.crt; ssl_certificate_key ${runtimeDirectory}/fixture.key;\nadd_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;\n${handoff}\nlocation / { return 404; }\n}\n}\n`;
      await writeFile(path.join(outputDirectory, filename), result, { flag: 'wx' });
      files.push(filename);
    }
  }
  await writeFile(path.join(outputDirectory, '.pinvou-nginx-fixture'), 'PUBLIC_LOOPBACK_FIXTURE_ONLY\n', { flag: 'wx' });
  return Object.freeze({ decision: 'FIXTURES_RENDERED', files, environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
}

async function main(args) {
  if (args.length !== 4 || args[0] !== '--output' || args[2] !== '--runtime-root') throw new Error('NGINX_FIXTURE_USAGE');
  process.stdout.write(`${JSON.stringify(await renderNginxFixtures(path.resolve(args[1]), args[3]))}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main(process.argv.slice(2)).catch(error => {
  process.stderr.write(`${/^NGINX_FIXTURE_[A-Z_]+$/.test(error?.message) ? error.message : 'NGINX_FIXTURE_FAILED'}\n`);
  process.exitCode = 1;
});
