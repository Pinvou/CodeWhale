package billing

import (
	"context"

	"github.com/google/uuid"
)

// A query is accepted before external I/O. Only its generation may publish the
// current archive-admission fact; historical query times and evidence stay put.
// This statement commits before Provider I/O and holds no lock across that I/O.
func (p *PaymentCoordinator) beginArchiveQueryObservation(ctx context.Context, order PaymentOrder) (uuid.UUID, error) {
	generation := uuid.New()
	command, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET archive_query_generation=$2,archive_query_authoritative_unpaid=NULL WHERE id=$1 AND provider_id=$3 AND provider_version=$4`, order.ID, generation, order.ProviderID, order.ProviderVersion)
	if err != nil {
		return uuid.Nil, err
	}
	if command.RowsAffected() != 1 {
		return uuid.Nil, ErrConflict
	}
	return generation, nil
}

// This runs only after the original query/close/financial flow has succeeded.
// Old generations still merge all their original observations and strong paid
// facts. Losing this fence suppresses only their current admission projection.
func (p *PaymentCoordinator) completeArchiveQueryObservation(ctx context.Context, order PaymentOrder, generation uuid.UUID, observation ProviderOrder, authoritative bool) error {
	unpaid := authoritative && (observation.Status == "CLOSED" || observation.Status == "FAILED") && providerMatchesOrder(observation, order)
	_, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET archive_query_authoritative_unpaid=$3 WHERE id=$1 AND archive_query_generation=$2`, order.ID, generation, unpaid)
	return err
}
