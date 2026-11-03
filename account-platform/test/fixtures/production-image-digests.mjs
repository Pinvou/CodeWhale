const productionImageNames = [
  'accountService', 'identityService', 'billingService', 'tokenGateway', 'tokenPortal',
  'tokenCredentialPortal', 'tokenCommandExecutor', 'tokenGovernance', 'eventRelay',
  'migrate', 'tokenPolicyBootstrap', 'keycloak', 'redis', 'nats',
];
const fixtureDigest = `sha256:${'0'.repeat(64)}`;

export const productionImageDigestArgs = productionImageNames.flatMap(name => [
  '--set-string', `images.${name}.digest=${fixtureDigest}`,
]);
