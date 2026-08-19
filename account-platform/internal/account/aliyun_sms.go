package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type SMSPurpose string

const (
	SMSPurposeRegister SMSPurpose = "REGISTER"
	SMSPurposeLogin    SMSPurpose = "LOGIN"
)

type SMSProvider interface {
	SendCode(context.Context, string, SMSPurpose, string) (string, error)
}

type AliyunSMSConfig struct {
	AccessKeyID      string
	AccessKeySecret  string
	SignName         string
	RegisterTemplate string
	LoginTemplate    string
	Endpoint         string
}

type AliyunSMSProvider struct {
	config AliyunSMSConfig
	client *http.Client
	now    func() time.Time
}

func NewAliyunSMSProvider(config AliyunSMSConfig) (*AliyunSMSProvider, error) {
	config.AccessKeyID = strings.TrimSpace(config.AccessKeyID)
	config.AccessKeySecret = strings.TrimSpace(config.AccessKeySecret)
	config.SignName = strings.TrimSpace(config.SignName)
	config.RegisterTemplate = strings.TrimSpace(config.RegisterTemplate)
	config.LoginTemplate = strings.TrimSpace(config.LoginTemplate)
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	if config.Endpoint == "" {
		config.Endpoint = "https://dysmsapi.aliyuncs.com"
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("ALIYUN_SMS_ENDPOINT must be an absolute HTTPS URL")
	}
	if config.AccessKeyID == "" || config.AccessKeySecret == "" || config.SignName == "" || config.RegisterTemplate == "" || config.LoginTemplate == "" {
		return nil, errors.New("Aliyun SMS credentials, sign name, and templates are required")
	}
	return &AliyunSMSProvider{config: config, client: observability.NewHTTPClient(10 * time.Second), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (p *AliyunSMSProvider) SendCode(ctx context.Context, mobile string, purpose SMSPurpose, code string) (string, error) {
	template := p.config.LoginTemplate
	if purpose == SMSPurposeRegister {
		template = p.config.RegisterTemplate
	}
	parameters := url.Values{
		"PhoneNumbers":  {strings.TrimPrefix(mobile, "+86")},
		"SignName":      {p.config.SignName},
		"TemplateCode":  {template},
		"TemplateParam": {fmt.Sprintf(`{"code":%q}`, code)},
	}
	request, err := p.signedRequest(ctx, parameters)
	if err != nil {
		return "", err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("send Aliyun SMS: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		Code      string `json:"Code"`
		Message   string `json:"Message"`
		BizID     string `json:"BizId"`
		RequestID string `json:"RequestId"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&body); err != nil {
		return "", errors.New("Aliyun SMS returned an invalid response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || body.Code != "OK" {
		return "", fmt.Errorf("Aliyun SMS rejected the request: %s", sanitizeProviderCode(body.Code))
	}
	if body.BizID != "" {
		return body.BizID, nil
	}
	return body.RequestID, nil
}

func (p *AliyunSMSProvider) signedRequest(ctx context.Context, parameters url.Values) (*http.Request, error) {
	endpoint, _ := url.Parse(p.config.Endpoint)
	endpoint.Path = "/"
	endpoint.RawQuery = canonicalQuery(parameters)
	emptyHash := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(emptyHash[:])
	date := p.now().Format("2006-01-02T15:04:05Z")
	nonce := uuid.NewString()
	signedHeaders := "host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version"
	canonicalHeaders := "host:" + endpoint.Host + "\n" +
		"x-acs-action:SendSms\n" +
		"x-acs-content-sha256:" + payloadHash + "\n" +
		"x-acs-date:" + date + "\n" +
		"x-acs-signature-nonce:" + nonce + "\n" +
		"x-acs-version:2017-05-25\n"
	canonicalRequest := "POST\n/\n" + endpoint.RawQuery + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "ACS3-HMAC-SHA256\n" + hex.EncodeToString(canonicalHash[:])
	mac := hmac.New(sha256.New, []byte(p.config.AccessKeySecret))
	_, _ = mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Host = endpoint.Host
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-acs-action", "SendSms")
	request.Header.Set("x-acs-content-sha256", payloadHash)
	request.Header.Set("x-acs-date", date)
	request.Header.Set("x-acs-signature-nonce", nonce)
	request.Header.Set("x-acs-version", "2017-05-25")
	request.Header.Set("Authorization", "ACS3-HMAC-SHA256 Credential="+p.config.AccessKeyID+",SignedHeaders="+signedHeaders+",Signature="+signature)
	return request, nil
}

func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		for _, value := range items {
			parts = append(parts, rfc3986(key)+"="+rfc3986(value))
		}
	}
	return strings.Join(parts, "&")
}

func rfc3986(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func sanitizeProviderCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 80 {
		return "UNKNOWN"
	}
	for _, r := range code {
		if !(r == '.' || r == '_' || r == '-' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return "UNKNOWN"
		}
	}
	return code
}
