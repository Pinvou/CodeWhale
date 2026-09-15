package account

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type approvalConsumerDeliveryRepository interface {
	ResolveApprovalAuthorizationBinding(context.Context, uuid.UUID, string, string) (ApprovalAuthorizationBinding, error)
	ObserveApprovalAuthorizationDelivery(context.Context, ApprovalDeliveryObservation) (ApprovalDeliveryState, error)
	GrantApprovalAuthorizationDomainAttempt(context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error)
	RecordApprovalAuthorizationDomainFailure(context.Context, uuid.UUID, string, string, int64, string) (ApprovalDeliveryState, error)
	RecordApprovalAuthorizationResult(context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error)
	RecordApprovalAuthorizationDLQPubAck(context.Context, uuid.UUID, string, string, int64, string, approvalconsumer.Sequence) (ApprovalDeliveryState, error)
	RecordApprovalAuthorizationTerminal(context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error)
}

func (s *Service) approvalConsumerDeliveryRepository(w http.ResponseWriter, r *http.Request) (approvalConsumerDeliveryRepository, bool) {
	repository, ok := s.repository.(approvalConsumerDeliveryRepository)
	if !ok {
		s.writeError(w, r, ErrApprovalConsumerBindingUnavailable)
	}
	return repository, ok
}

func (s *Service) approvalConsumerIdentity(w http.ResponseWriter, r *http.Request) (approvalConsumerDeliveryRepository, uuid.UUID, string, string, bool) {
	repository, ok := s.approvalConsumerDeliveryRepository(w, r)
	if !ok {
		return nil, uuid.Nil, "", "", false
	}
	eventID, ok := pathUUID(w, r, "event_id")
	if !ok {
		return nil, uuid.Nil, "", "", false
	}
	durable := r.PathValue("durable")
	if durable == "" {
		durable = r.URL.Query().Get("durable")
	}
	workload, ok := approvalExecutorFromContext(w, r)
	if !ok || !boundedApprovalConsumerString(durable, 120) {
		if ok {
			s.writeError(w, r, ErrApprovalConsumerBindingInvalid)
		}
		return nil, uuid.Nil, "", "", false
	}
	return repository, eventID, durable, workload, true
}

func (s *Service) resolveApprovalAuthorizationBinding(w http.ResponseWriter, r *http.Request) {
	repository, eventID, durable, workload, ok := s.approvalConsumerIdentity(w, r)
	if !ok {
		return
	}
	binding, err := repository.ResolveApprovalAuthorizationBinding(r.Context(), eventID, durable, workload)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, binding)
}

func (s *Service) observeApprovalAuthorizationDelivery(w http.ResponseWriter, r *http.Request) {
	repository, eventID, durable, workload, ok := s.approvalConsumerIdentity(w, r)
	if !ok {
		return
	}
	var body struct {
		ApprovalRequestID      uuid.UUID                 `json:"approval_request_id"`
		ApprovedRequestVersion int64                     `json:"approved_request_version"`
		ApprovalEpoch          int64                     `json:"approval_epoch"`
		SerializationVersion   string                    `json:"serialization_version"`
		Provenance             string                    `json:"provenance"`
		EventDigest            string                    `json:"event_digest"`
		DigestKind             string                    `json:"digest_kind"`
		CommandType            string                    `json:"command_type"`
		PolicyVersion          int64                     `json:"policy_version"`
		ExecutorIdentity       string                    `json:"executor_identity"`
		Stream                 string                    `json:"stream"`
		StreamSequence         approvalconsumer.Sequence `json:"stream_sequence"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_AUTHORIZATION_DELIVERY", "invalid approval authorization delivery request", httpserver.RequestID(r.Context()))
		return
	}
	binding := ApprovalAuthorizationBinding{
		EventID: eventID, Durable: durable, Workload: workload, ApprovalRequestID: body.ApprovalRequestID,
		ApprovedRequestVersion: body.ApprovedRequestVersion, ApprovalEpoch: body.ApprovalEpoch,
		SerializationVersion: body.SerializationVersion, Provenance: body.Provenance, EventDigest: body.EventDigest,
		DigestKind: body.DigestKind, CommandType: body.CommandType, PolicyVersion: body.PolicyVersion,
		ExecutorIdentity: body.ExecutorIdentity,
	}
	state, err := repository.ObserveApprovalAuthorizationDelivery(r.Context(), ApprovalDeliveryObservation{Binding: binding, Stream: body.Stream, StreamSequence: body.StreamSequence})
	s.writeApprovalDeliveryState(w, r, state, err)
}

func (s *Service) grantApprovalAuthorizationDomainAttempt(w http.ResponseWriter, r *http.Request) {
	s.approvalDeliveryVersionAction(w, r, func(repository approvalConsumerDeliveryRepository, ctx context.Context, eventID uuid.UUID, durable, workload string, version int64) (ApprovalDeliveryState, error) {
		return repository.GrantApprovalAuthorizationDomainAttempt(ctx, eventID, durable, workload, version)
	})
}

func (s *Service) recordApprovalAuthorizationResult(w http.ResponseWriter, r *http.Request) {
	s.approvalDeliveryVersionAction(w, r, func(repository approvalConsumerDeliveryRepository, ctx context.Context, eventID uuid.UUID, durable, workload string, version int64) (ApprovalDeliveryState, error) {
		return repository.RecordApprovalAuthorizationResult(ctx, eventID, durable, workload, version)
	})
}

func (s *Service) recordApprovalAuthorizationTerminal(w http.ResponseWriter, r *http.Request) {
	s.approvalDeliveryVersionAction(w, r, func(repository approvalConsumerDeliveryRepository, ctx context.Context, eventID uuid.UUID, durable, workload string, version int64) (ApprovalDeliveryState, error) {
		return repository.RecordApprovalAuthorizationTerminal(ctx, eventID, durable, workload, version)
	})
}

func (s *Service) approvalDeliveryVersionAction(w http.ResponseWriter, r *http.Request, action func(approvalConsumerDeliveryRepository, context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error)) {
	repository, eventID, durable, workload, ok := s.approvalConsumerIdentity(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_AUTHORIZATION_DELIVERY", "invalid approval authorization delivery request", httpserver.RequestID(r.Context()))
		return
	}
	state, err := action(repository, r.Context(), eventID, durable, workload, body.ExpectedVersion)
	s.writeApprovalDeliveryState(w, r, state, err)
}

func (s *Service) recordApprovalAuthorizationDomainFailure(w http.ResponseWriter, r *http.Request) {
	repository, eventID, durable, workload, ok := s.approvalConsumerIdentity(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion int64  `json:"expected_version"`
		ErrorCode       string `json:"error_code"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_AUTHORIZATION_DELIVERY", "invalid approval authorization delivery request", httpserver.RequestID(r.Context()))
		return
	}
	state, err := repository.RecordApprovalAuthorizationDomainFailure(r.Context(), eventID, durable, workload, body.ExpectedVersion, body.ErrorCode)
	s.writeApprovalDeliveryState(w, r, state, err)
}

func (s *Service) recordApprovalAuthorizationDLQPubAck(w http.ResponseWriter, r *http.Request) {
	repository, eventID, durable, workload, ok := s.approvalConsumerIdentity(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion int64                     `json:"expected_version"`
		MessageID       string                    `json:"message_id"`
		StreamSequence  approvalconsumer.Sequence `json:"stream_sequence"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_AUTHORIZATION_DELIVERY", "invalid approval authorization delivery request", httpserver.RequestID(r.Context()))
		return
	}
	state, err := repository.RecordApprovalAuthorizationDLQPubAck(r.Context(), eventID, durable, workload, body.ExpectedVersion, body.MessageID, body.StreamSequence)
	s.writeApprovalDeliveryState(w, r, state, err)
}

func (s *Service) writeApprovalDeliveryState(w http.ResponseWriter, r *http.Request, state ApprovalDeliveryState, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, state)
}
