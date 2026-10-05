package commerce

import "context"

type CustomerProvisionRequest struct {
	SubjectType   string
	SubjectID     string
	ContactMobile string
}

type CustomerProvisionResult struct {
	ExternalReferenceRedacted string
}

type CustomerAdapter interface {
	EnsureCustomer(context.Context, CustomerProvisionRequest) (CustomerProvisionResult, error)
}
