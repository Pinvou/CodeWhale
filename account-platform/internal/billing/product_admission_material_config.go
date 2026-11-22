package billing

import (
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/transit"
)

// ProductAdmissionMaterialConfig enables the existing isolated validator and
// workload-specific Transit client. It never accepts a local encryption key.
type ProductAdmissionMaterialConfig struct {
	Transit         transit.Config
	ValidatorSocket string
	ValidatorUID    uint32
}

func (s *Service) WithProductAdmissionEnvironment(environment string) *Service {
	s.productAdmissionEnvironment = environment
	return s
}

func ConfigureProductAdmissionMaterials(service *Service, repository ProductAdmissionMaterialRepository, config ProductAdmissionMaterialConfig) error {
	if service == nil || repository == nil {
		return errors.New("product admission material dependencies are required")
	}
	validator, err := newEvidenceImageValidator(config.ValidatorSocket, config.ValidatorUID)
	if err != nil {
		return errors.New("product admission isolated validator configuration is invalid")
	}
	wrapper, err := transit.NewTransitProvider(config.Transit)
	if err != nil {
		return errors.New("product admission material key configuration is invalid")
	}
	service.WithProductAdmissionMaterials(repository, wrapper, validator)
	return nil
}
