import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const root = new URL('../../', import.meta.url);
const read = path => readFileSync(new URL(path, root), 'utf8');

test('model catalog preserves stable identity, immutable history, lifecycle, and request version snapshots', () => {
  const migration = read('migrations/token/000012_model_catalog_lifecycle.up.sql');
  const catalog = read('internal/token/catalog.go');
  const proxy = read('internal/token/proxy.go');
  const service = read('internal/token/service.go');
  const publicAPI = read('contracts/openapi/token-gateway.yaml');
  const adminAPI = read('contracts/openapi/admin.yaml');
  const ui = read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx');
  const client = read('../pinvou3-app/src/features/account-platform/admin-client.js');
  const i18n = read('../pinvou3-app/src/shared/admin-platform-i18n.js');
  const compose = read('deploy/compose/compose.yaml');

  for (const table of ['model_identities', 'model_capability_versions', 'model_catalog_versions', 'model_catalog_entries', 'model_catalog_drafts', 'model_catalog_commands', 'model_catalog_audit']) assert.match(migration, new RegExp(`CREATE TABLE ${table}`));
  assert.match(migration, /published model catalog history is immutable/);
  assert.match(migration, /UNIQUE\(provider_id,upstream_model_ref_digest\)/);
  assert.match(migration, /DRAFT|PUBLISHED/);
  assert.match(migration, /DEPRECATED.*REMOVED/);
  for (const field of ['model_id', 'catalog_version', 'model_capability_version_id', 'protocol_definition_id', 'route_generation', 'model_snapshot']) assert.match(migration, new RegExp(`ADD COLUMN ${field}`));
  assert.match(catalog, /DRAFT[\s\S]*PUBLISHED/);
  assert.match(catalog, /PUBLISHED[\s\S]*DEPRECATED[\s\S]*REMOVED/);
  assert.match(catalog, /source_missing_confirmations|SourceMissingConfirmations/);
  assert.match(catalog, /rollback_source_version|RollbackCatalog/);
  assert.match(catalog, /model-catalog-publication/);
  assert.match(catalog, /WHERE provider_id=\$1 AND upstream_model_ref_digest=\$2/);
  assert.match(catalog, /ErrCatalogAliasConflict/);
  assert.match(proxy, /ResolveCatalogModel/);
  for (const field of ['catalog_version', 'model_capability_version_id', 'protocol_definition_id', 'formula_version_id', 'price_version_id', 'route_generation']) assert.match(proxy, new RegExp(field));
  assert.match(service, /\/v1\/models/);
  assert.match(publicAPI, /\/models:[\s\S]*CatalogModel/);
  assert.match(adminAPI, /models\/{model_id\}\/draft[\s\S]*models\/{model_id\}\/publish[\s\S]*model-catalog:rollback/);
  assert.match(ui, /modelCatalog/);
  assert.match(client, /syncModelCatalog[\s\S]*rollbackModelCatalog/);
  for (const language of ['模型目录', 'Model catalog', 'モデルカタログ']) assert.match(i18n, new RegExp(language));
  assert.match(compose, /TOKEN_MODEL_CATALOG_SYNC_MINUTES/);
});
