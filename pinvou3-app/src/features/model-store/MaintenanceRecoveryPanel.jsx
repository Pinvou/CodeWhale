import { MsButton } from './model-store-ui.jsx';

const EMPTY_TRANSACTIONS = Object.freeze([]);

export function MaintenanceRecoveryPanel({ transactions = EMPTY_TRANSACTIONS, error, copy, onRecover, onRefresh, isRecovering }) {
  if (transactions.length === 0 && !error) return null;
  return (
    <section data-testid="model-maintenance-recovery" className="mx-3 mb-5 rounded-2xl border border-amber-300/50 bg-amber-50/60 p-4 dark:bg-amber-950/20">
      <h3 className="text-sm font-semibold">{copy.maintenanceTitle}</h3>
      <p className="mt-1 text-xs opacity-75">{copy.maintenanceHint}</p>
      {error ? <p role="alert" className="mt-2 break-words text-xs">{error}</p> : null}
      {transactions.map((transaction) => (
        <div key={transaction.cardId} data-testid="model-maintenance-item" className="mt-3 flex flex-wrap items-center justify-between gap-3">
          <div className="min-w-0 text-xs">
            <div className="font-semibold">{transaction.cardName || transaction.cardId}</div>
            {transaction.reservationOnly ? <div className="mt-1 opacity-75">{copy.maintenanceReservation}</div> : null}
            <div className="mt-1 opacity-75">{transaction.workerActive ? copy.maintenanceWorking : copy.maintenancePending}</div>
            {transaction.error ? <div className="mt-1 break-words">{transaction.error}</div> : null}
          </div>
          <MsButton disabled={transaction.workerActive || transaction.phase === 'record_error' || isRecovering?.(transaction.cardId)} onClick={() => onRecover(transaction)}>
            {transaction.reservationOnly ? copy.maintenanceReservationRecover : copy.maintenanceRecover}
          </MsButton>
        </div>
      ))}
      <MsButton tone="ghost" className="mt-3" onClick={onRefresh}>{copy.refresh}</MsButton>
    </section>
  );
}
