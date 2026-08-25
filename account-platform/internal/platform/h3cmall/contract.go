package h3cmall

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Decimal preserves the Mall's decimal money values without passing through
// binary floating point. The Mall currently emits JSON numbers, while quoted
// decimal strings are accepted for compatibility with equivalent serializers.
type Decimal string

func (d *Decimal) UnmarshalJSON(body []byte) error {
	body = bytes.TrimSpace(body)
	if bytes.Equal(body, []byte("null")) {
		*d = ""
		return nil
	}
	var value string
	if len(body) > 0 && body[0] == '"' {
		if err := json.Unmarshal(body, &value); err != nil {
			return err
		}
	} else {
		value = string(body)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("Mall decimal value is empty")
	}
	if _, ok := new(big.Rat).SetString(value); !ok {
		return errors.New("Mall decimal value is invalid")
	}
	*d = Decimal(value)
	return nil
}

func (d Decimal) Fen() (int64, error) {
	value := strings.TrimSpace(string(d))
	if value == "" {
		return 0, nil
	}
	rational, ok := new(big.Rat).SetString(value)
	if !ok || rational.Sign() < 0 {
		return 0, errors.New("Mall amount is invalid")
	}
	scaled := new(big.Rat).Mul(rational, big.NewRat(100, 1))
	if !scaled.IsInt() || !scaled.Num().IsInt64() {
		return 0, errors.New("Mall amount must have at most two decimal places")
	}
	return scaled.Num().Int64(), nil
}

type Order struct {
	ID                  *int64  `json:"id"`
	OrderKey            string  `json:"orderKey"`
	MerchantCustomerKey string  `json:"merchantCustomerKey"`
	Mobile              string  `json:"mobile"`
	SKU                 string  `json:"skuBom"`
	TradeOrderCode      string  `json:"tradeOrderCode"`
	TradeOrderID        *int64  `json:"tradeOrderId"`
	SKUID               *int64  `json:"skuId"`
	NeedPayAmount       Decimal `json:"needPayAmt"`
	PaidAmount          Decimal `json:"paidAmt"`
	PaymentStatus       string  `json:"paymentStatus"`
	PayEndTime          any     `json:"payEndTime"`
	SKUAmount           Decimal `json:"skuAmt"`
	ReceiveAddressID    *int64  `json:"receiveAddressId"`
	ShopID              *int64  `json:"shopId"`
	TradeContractID     *int64  `json:"tradeContractId"`
	TradeContractCode   string  `json:"tradeContractCode"`
	PayUniqueKey        string  `json:"payUniqueKey"`
	PaymentOrderID      *int64  `json:"paymentOrderId"`
	BuyerID             string  `json:"buyerId"`
	PayChannel          string  `json:"payChannel"`
	AliQRData           string  `json:"aliQrData"`
	WechatQRData        string  `json:"wechatQrData"`
	Status              string  `json:"status"`
	Node                string  `json:"node"`
	PayTime             any     `json:"payTime"`
	ExecuteCostMS       *int64  `json:"executeCostMs"`
	ErrorMessage        string  `json:"errorMsg"`
	TraceID             string  `json:"traceId"`
	CreatedAt           any     `json:"createdAt"`
	UpdatedAt           any     `json:"updatedAt"`
}

type PageQueryRequest struct {
	MerchantCustomerKey string
	Mobile              string
	OrderKey            string
	SKU                 string
	Status              string
	Current             int
	PageSize            int
}

type OrderPage struct {
	Records []Order `json:"records"`
	Total   int64   `json:"total"`
	Size    int64   `json:"size"`
	Current int64   `json:"current"`
	Pages   int64   `json:"pages"`
}

func ParseTimeValue(value any) (time.Time, error) {
	switch typed := value.(type) {
	case nil:
		return time.Time{}, errors.New("Mall time is missing")
	case string:
		return parseTimeString(typed)
	case float64:
		if typed <= 0 || math.Trunc(typed) != typed {
			return time.Time{}, errors.New("Mall timestamp is invalid")
		}
		return parseEpoch(int64(typed))
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return time.Time{}, errors.New("Mall timestamp is invalid")
		}
		return parseEpoch(parsed)
	default:
		return time.Time{}, errors.New("Mall timestamp has an unsupported type")
	}
}

func parseTimeString(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("Mall time is missing")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, location)
	if err != nil {
		return time.Time{}, errors.New("Mall time is invalid")
	}
	return parsed.UTC(), nil
}

func parseEpoch(value int64) (time.Time, error) {
	if value <= 0 {
		return time.Time{}, errors.New("Mall timestamp is invalid")
	}
	if value >= 1_000_000_000_000 {
		return time.UnixMilli(value).UTC(), nil
	}
	return time.Unix(value, 0).UTC(), nil
}
