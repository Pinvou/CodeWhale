package billing

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
)

// Admin reads are a separate wire view. This never changes stored snapshots,
// signed approval documents, or the internal wallet authority contract.
func writeAdminFinancialJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err == nil {
		raw, err = adminFinancialSnapshot(raw)
	}
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, "FINANCIAL_VIEW_UNAVAILABLE", "the financial view is unavailable", "")
		return
	}
	api.WriteJSON(w, status, json.RawMessage(raw))
}

func adminFinancialSnapshot(snapshot json.RawMessage) (json.RawMessage, error) {
	value := strings.TrimSpace(string(snapshot))
	if value == "" || value == "null" {
		return snapshot, nil
	}
	if strings.HasPrefix(value, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(snapshot, &items); err != nil {
			return nil, err
		}
		for i := range items {
			converted, err := adminFinancialSnapshot(items[i])
			if err != nil {
				return nil, err
			}
			items[i] = converted
		}
		return json.Marshal(items)
	}
	if !strings.HasPrefix(value, "{") {
		return snapshot, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(snapshot, &fields); err != nil {
		return nil, err
	}
	for name, field := range fields {
		trimmed := strings.TrimSpace(string(field))
		if (strings.HasSuffix(name, "_fen") || strings.HasSuffix(name, "_microcredits")) && trimmed != "null" {
			// Preserve already exact strings, including SQL aggregate totals.
			if strings.HasPrefix(trimmed, `"`) {
				continue
			}
			amount, err := strconv.ParseInt(trimmed, 10, 64)
			if err != nil {
				return nil, errors.New("invalid financial snapshot")
			}
			fields[name], _ = json.Marshal(strconv.FormatInt(amount, 10))
			continue
		}
		converted, err := adminFinancialSnapshot(field)
		if err != nil {
			return nil, err
		}
		fields[name] = converted
	}
	return json.Marshal(fields)
}

// Money metrics use the same exact wire values as financial rows. Counts retain
// their ordinary number contract, so the unit remains the discriminator.
func (metric FinanceOverviewMetric) MarshalJSON() ([]byte, error) {
	type wire struct {
		Key              string   `json:"key"`
		Value            any      `json:"value"`
		Unit             string   `json:"unit"`
		IncludedStatuses []string `json:"included_statuses"`
	}
	var value any = metric.Value
	if metric.Unit == "FEN" || metric.Unit == "CNY_FEN" || metric.Unit == "MICROCREDITS" {
		value = strconv.FormatInt(metric.Value, 10)
	}
	return json.Marshal(wire{metric.Key, value, metric.Unit, metric.IncludedStatuses})
}
