import assert from 'node:assert/strict';
import test from 'node:test';
import { checkObservabilitySource } from '../../../scripts/check-account-platform-observability.mjs';

test('observability release gate rejects logger bypasses and protected attributes', () => {
  assert.deepEqual(checkObservabilitySource('internal/example.go', 'logger.Info("ok", "status", 200)'), []);
  assert.match(checkObservabilitySource('internal/example.go', 'slog.New(slog.NewJSONHandler(os.Stdout, nil))').join('\n'), /bypasses/);
  assert.match(checkObservabilitySource('internal/example.go', 'slog.Error("failed", "password", value)').join('\n'), /protected/);
  assert.match(checkObservabilitySource('internal/example.go', 'logger.Error("failed", "request_body", body)').join('\n'), /protected/);
  assert.match(checkObservabilitySource('internal/example.go', 'api.WriteError(w, 500, "FAILED", err.Error(), requestID)').join('\n'), /unclassified/);
  assert.match(checkObservabilitySource('internal/example.go', 'store.RescheduleReconciliation(ctx, id, err.Error(), next)').join('\n'), /unclassified/);
  assert.match(checkObservabilitySource('internal/example.go', 'span.RecordError(err)').join('\n'), /trace/);
  assert.match(checkObservabilitySource('internal/example.go', 'otelhttp.NewTransport(base)').join('\n'), /trace attributes/);
});
