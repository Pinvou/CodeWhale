package billing

import "strconv"

// Write commands and the Token pricing protocol retain safe integer fields.
// Administration reads expose financial values as exact decimal strings.
type AdminPriceUnitConfiguration struct {
	UnitDefinitionID         string  `json:"unit_definition_id"`
	ConfigType               string  `json:"config_type"`
	FixedPricePerDenominator *string `json:"fixed_price_per_denominator,omitempty"`
}

type AdminManagedPriceVersion struct {
	ManagedPriceVersion
	Units []AdminPriceUnitConfiguration `json:"units"`
}

func managedPriceResponse(item ManagedPriceVersion) AdminManagedPriceVersion {
	units := make([]AdminPriceUnitConfiguration, 0, len(item.Units))
	for _, unit := range item.Units {
		view := AdminPriceUnitConfiguration{UnitDefinitionID: unit.UnitDefinitionID.String(), ConfigType: unit.ConfigType}
		if unit.FixedPricePerDenominator != nil {
			value := strconv.FormatInt(*unit.FixedPricePerDenominator, 10)
			view.FixedPricePerDenominator = &value
		}
		units = append(units, view)
	}
	return AdminManagedPriceVersion{ManagedPriceVersion: item, Units: units}
}

type AdminResolvedUnitPrice struct {
	ResolvedUnitPrice
	ResolvedPrice string `json:"resolved_price"`
}
type AdminResolvedPriceVersion struct {
	PriceVersion
	InputPerMillion       string                   `json:"input_per_million"`
	OutputPerMillion      string                   `json:"output_per_million"`
	CachedInputPerMillion string                   `json:"cached_input_per_million"`
	Units                 []AdminResolvedUnitPrice `json:"units"`
}

func resolvedPriceResponse(item PriceVersion) AdminResolvedPriceVersion {
	units := make([]AdminResolvedUnitPrice, 0, len(item.Units))
	for _, unit := range item.Units {
		units = append(units, AdminResolvedUnitPrice{ResolvedUnitPrice: unit, ResolvedPrice: strconv.FormatInt(unit.ResolvedPrice, 10)})
	}
	return AdminResolvedPriceVersion{PriceVersion: item, InputPerMillion: strconv.FormatInt(item.InputPerMillion, 10), OutputPerMillion: strconv.FormatInt(item.OutputPerMillion, 10), CachedInputPerMillion: strconv.FormatInt(item.CachedInputPerMillion, 10), Units: units}
}
