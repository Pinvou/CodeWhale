import assert from 'node:assert/strict';

// Expectations are independent of the production PromQL. Read only the two
// configured numeric thresholds from the real render so non-default values
// run the same boundary, replica, hold-time and failure scenarios.
export function retentionRuleCases(groups) {
  const rules = groups.flatMap(group => group.rules);
  const names = ['PinvouOutboxStatisticsUnavailable', 'PinvouOutboxRetainedStorageWarning',
    'PinvouPaymentHealthStorageUnavailable', 'PinvouPaymentHealthRetainedStorageWarning'];
  const selected = names.map(name => {
    const matches = rules.filter(rule => rule.alert === name);
    assert.equal(matches.length, 1, `render ${name} exactly once`);
    return matches[0];
  });
  const threshold = rule => {
    const value = Number(/> (\d+)$/.exec(rule.expr)?.[1]);
    assert.ok(Number.isSafeInteger(value) && value > 0, 'finite positive byte threshold');
    return value;
  };
  const outbox = threshold(selected[1]); const health = threshold(selected[3]);
  const series = (name, source, values, instance = 'a1') => ({
    series: `${name}{source="${source}",instance="${instance}"}`, values,
  });
  const flat = (name, source, value, instance) => series(name, source, `${value}+0x20`, instance);
  const available = (source, value = 1, instance) => flat('pinvou_outbox_statistics_available', source, value, instance);
  const fresh = (source, values = '0+60x20', instance) => series('pinvou_outbox_statistics_last_success_timestamp_seconds', source, values, instance);
  const healthy = [available('account'), available('billing'), fresh('account'), fresh('billing')];
  const check = (index, labels = null, eval_time = '13m') => ({
    alertname: names[index], eval_time,
    exp_alerts: labels === null ? [] : [{ exp_labels: { severity: index % 2 === 0 ? 'p1' : 'p2', ...labels },
      exp_annotations: selected[index].annotations }],
  });
  const cases = [
    ['fresh-statistics', healthy, [check(0)]],
    ['failed-replica-is-visible', [...healthy, available('account', 0, 'a2')], [check(0, { source: 'account' })]],
    ['stale-success-is-visible', [available('account'), available('billing'), fresh('account', '0+0x20'), fresh('billing')], [check(0, { source: 'account' })]],
    ['missing-billing-is-visible', [available('account'), fresh('account')], [check(0, { source: 'billing' })]],
    ['unavailable-before-hold', [...healthy, available('account', 0, 'a2')], [check(0, null, '1m')]],
    ['outbox-replicas-are-not-summed', [flat('pinvou_outbox_retained_storage_bytes', 'account', outbox, 'a1'), flat('pinvou_outbox_retained_storage_bytes', 'account', outbox, 'a2')], [check(1)]],
    ['outbox-over-threshold', [flat('pinvou_outbox_retained_storage_bytes', 'billing', outbox + 1)], [check(1, { source: 'billing' })]],
    ['outbox-warning-before-hold', [flat('pinvou_outbox_retained_storage_bytes', 'billing', outbox + 1)], [check(1, null, '9m')]],
    ['health-storage-healthy', [flat('pinvou_payment_health_storage_available', 'billing', 1)], [check(2)]],
    ['health-storage-failed-replica', [flat('pinvou_payment_health_storage_available', 'billing', 1), flat('pinvou_payment_health_storage_available', 'billing', 0, 'b2')], [check(2, {})]],
    ['health-storage-missing', [], [check(2, {})]],
    ['health-replicas-are-not-summed', [flat('pinvou_payment_health_retained_storage_bytes', 'billing', health, 'a1'), flat('pinvou_payment_health_retained_storage_bytes', 'billing', health, 'a2')], [check(3)]],
    ['health-over-threshold', [flat('pinvou_payment_health_retained_storage_bytes', 'billing', health + 1)], [check(3, {})]],
    ['health-warning-before-hold', [flat('pinvou_payment_health_retained_storage_bytes', 'billing', health + 1)], [check(3, null, '9m')]],
    ['statistics-recover', [available('billing'), fresh('billing'), series('pinvou_outbox_statistics_available', 'account', '0+0x3 1+0x16'), fresh('account')], [check(0)]],
    ['health-storage-recovers', [series('pinvou_payment_health_storage_available', 'billing', '0+0x3 1+0x16')], [check(2)]],
  ];
  return cases.map(([name, input_series, alert_rule_test]) => ({ name: `retention-${name}`, interval: '1m', input_series, alert_rule_test }));
}
