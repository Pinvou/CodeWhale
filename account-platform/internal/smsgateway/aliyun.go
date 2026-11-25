package smsgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const smsHost = "dysmsapi.aliyuncs.com"
const signedHeaders = "host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version"
const maxResponseBytes = 32 << 10

type Aliyun struct {
	accessKeyID     string
	accessKeySecret string
	signName        string
	client          *http.Client
}

func NewAliyun(accessKeyID, accessKeySecret, signName string) (*Aliyun, error) {
	if accessKeyID == "" || accessKeySecret == "" || signName == "" || len(signName) > 100 ||
		strings.ContainsAny(accessKeyID, ", \t\r\n") || strings.ContainsAny(accessKeySecret+signName, "\r\n") {
		return nil, errors.New("invalid Aliyun SMS credentials or sign name")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Aliyun{accessKeyID: accessKeyID, accessKeySecret: accessKeySecret, signName: signName,
		client: &http.Client{
			Timeout:       7 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}, nil
}

func (a *Aliyun) send(ctx context.Context, input request, template string) sendResult {
	param, _ := json.Marshal(map[string]string{"code": input.Code})
	query := url.Values{
		"PhoneNumbers":  {strings.TrimPrefix(input.Mobile, "+86")},
		"SignName":      {a.signName},
		"TemplateCode":  {template},
		"TemplateParam": {string(param)},
	}
	req, err := a.signedRequest(ctx, query, time.Now().UTC(), uuid.NewString())
	if err != nil {
		return sendResult{}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return sendResult{}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return sendResult{}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return sendResult{}
	}
	var result struct {
		Code, BizID string
	}
	if json.Unmarshal(body, &result) != nil {
		return sendResult{}
	}
	switch result.Code {
	case "OK":
		return sendResult{MessageID: result.BizID}
	case "isv.MOBILE_NUMBER_ILLEGAL", "isv.SMS_TEMPLATE_ILLEGAL", "isv.SMS_SIGNATURE_ILLEGAL", "isv.TEMPLATE_MISSING_PARAMETERS", "isv.BUSINESS_LIMIT_CONTROL", "isv.BALANCE_NOT_ENOUGH":
		return sendResult{Rejected: true}
	default:
		return sendResult{}
	}
}

func (a *Aliyun) signedRequest(ctx context.Context, query url.Values, now time.Time, nonce string) (*http.Request, error) {
	endpoint := url.URL{Scheme: "https", Host: smsHost, Path: "/", RawQuery: strings.ReplaceAll(query.Encode(), "+", "%20")}
	emptyHash := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(emptyHash[:])
	date := now.UTC().Format("2006-01-02T15:04:05Z")
	headers := "host:" + smsHost + "\n" +
		"x-acs-action:SendSms\n" +
		"x-acs-content-sha256:" + payloadHash + "\n" +
		"x-acs-date:" + date + "\n" +
		"x-acs-signature-nonce:" + nonce + "\n" +
		"x-acs-version:2017-05-25\n"
	canonical := "POST\n/\n" + endpoint.RawQuery + "\n" + headers + "\n" + signedHeaders + "\n" + payloadHash
	canonicalHash := sha256.Sum256([]byte(canonical))
	mac := hmac.New(sha256.New, []byte(a.accessKeySecret))
	_, _ = mac.Write([]byte("ACS3-HMAC-SHA256\n" + hex.EncodeToString(canonicalHash[:])))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Acs-Action", "SendSms")
	req.Header.Set("X-Acs-Content-Sha256", payloadHash)
	req.Header.Set("X-Acs-Date", date)
	req.Header.Set("X-Acs-Signature-Nonce", nonce)
	req.Header.Set("X-Acs-Version", "2017-05-25")
	req.Header.Set("Authorization", "ACS3-HMAC-SHA256 Credential="+a.accessKeyID+",SignedHeaders="+signedHeaders+",Signature="+hex.EncodeToString(mac.Sum(nil)))
	return req, nil
}
