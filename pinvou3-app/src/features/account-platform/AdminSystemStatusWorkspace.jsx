import React, { useEffect, useRef, useState } from "react";
import { localizedAccountPlatformError } from "./error-localization.js";
import { formatSystemCount, requireSystemStatus } from "./admin-system-status.js";
const NO_PERMISSIONS = [];

function ReadTable({ title, columns, items, copy }) {
  return (
    <article className="adm-panel">
      <h2>{title}</h2>
      {items.length === 0 ? (
        <p>{copy.noData}</p>
      ) : (
        <div className="adm-table-wrap">
          <table className="adm-table">
            <thead>
              <tr>
                {columns.map(([key, label]) => (
                  <th key={key}>{label}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {items.map((item, index) => (
                <tr key={`${item.key || item.source}:${index}`}>
                  {columns.map(([key, label, render]) => (
                    <td key={key} data-label={label}>
                      {render ? render(item[key], item) : item[key] || "—"}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </article>
  );
}

export function AdminSystemStatusWorkspace({
  client,
  copy,
  language,
  permissions = NO_PERMISSIONS,
}) {
  const allowed = permissions.includes("platform.system.status.read");
  const [state, setState] = useState({ owner: null, status: null, loading: false, error: null });
  const [refresh, setRefresh] = useState(0);
  const generation = useRef(0);
  useEffect(() => {
    const request = ++generation.current;
    if (!allowed) {
      setState({ owner: client, status: null, loading: false, error: null });
      return undefined;
    }
    const controller = new AbortController();
    setState({ owner: client, status: null, loading: true, error: null });
    void client
      .systemStatus(controller.signal)
      .then(requireSystemStatus)
      .then((status) => {
        if (!controller.signal.aborted && generation.current === request)
          setState({ owner: client, status, loading: false, error: null });
      })
      .catch((error) => {
        if (!controller.signal.aborted && generation.current === request)
          setState({ owner: client, status: null, loading: false, error });
      });
    return () => {
      controller.abort();
      generation.current++;
    };
  }, [client, allowed, refresh]);
  if (!allowed) return <p role="alert">{copy.permissionDenied}</p>;
  const visible = state.owner === client ? state : { status: null, loading: true, error: null };
  const labels = copy.system;
  const count = (value) => formatSystemCount(value, language);
  const name = (value) => labels.names[value] || value;
  const statusCell = (value) => (
    <span className={`adm-status ${value === "HEALTHY" ? "ok" : "warn"}`}>{value}</span>
  );
  const age = (value) =>
    typeof value === "number" && Number.isFinite(value) && value >= 0
      ? `${value.toLocaleString(language)} s`
      : "—";
  const date = (value) =>
    value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString(language) : "—";
  const status = visible.status;
  return (
    <section className="adm-system-workspace">
      <h1>{copy.nav.system}</h1>
      <button
        type="button"
        disabled={visible.loading}
        onClick={() => setRefresh((value) => value + 1)}
      >
        {copy.refresh}
      </button>
      {visible.loading && <p role="status">{copy.loading}</p>}
      {visible.error && (
        <div role="alert">
          {localizedAccountPlatformError(copy, visible.error)}
          <button type="button" onClick={() => setRefresh((value) => value + 1)}>
            {copy.retry}
          </button>
        </div>
      )}
      {status && (
        <>
          <div className="adm-metrics">
            {[
              [
                labels.healthyServices,
                count(status.services.filter((item) => item.status === "HEALTHY").length),
              ],
              [labels.pendingReconciliation, count(status.reconciliation.pending)],
              [labels.stoppedReconciliation, count(status.reconciliation.stopped)],
            ].map(([label, value]) => (
              <article key={label}>
                <span>{label}</span>
                <strong>{value}</strong>
              </article>
            ))}
          </div>
          <div className="adm-system-sections">
            <ReadTable
              title={labels.services}
              copy={copy}
              items={status.services}
              columns={[
                ["key", labels.service, name],
                ["status", copy.accounts.status, statusCell],
                [
                  "latency_ms",
                  labels.latency,
                  (value) => (value === undefined ? "—" : `${count(value)} ms`),
                ],
                ["message", labels.message],
              ]}
            />
            <ReadTable
              title={labels.relays}
              copy={copy}
              items={status.relays}
              columns={[
                ["key", labels.service, name],
                ["status", copy.accounts.status, statusCell],
                ["pending_events", labels.pending, count],
                ["dead_lettered_events", labels.deadLetters, count],
                ["oldest_pending_seconds", labels.oldest, age],
                ["message", labels.message],
              ]}
            />
            <ReadTable
              title={labels.recentErrors}
              copy={copy}
              items={status.recent_errors}
              columns={[
                ["source", labels.source, name],
                ["resource", labels.resource, (value) => <code>{value || "—"}</code>],
                ["message", labels.message],
                ["updated_at", labels.updated, date],
              ]}
            />
          </div>
        </>
      )}
      <section className="adm-panel adm-runbook-panel">
        <h2>{copy.runbooks.title}</h2>
        <p>{copy.runbooks.lead}</p>
        {["bootstrap", "idp"].map((key) => (
          <details key={key}>
            <summary>
              <strong>{copy.runbooks[key]}</strong>
              <span className="adm-status neutral">OUT_OF_BAND</span>
            </summary>
            <p>{copy.runbooks[`${key}Help`]}</p>
            <p>{copy.runbooks.noExecution}</p>
          </details>
        ))}
      </section>
    </section>
  );
}
