package smsgateway

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// 下方黄金向量由独立于本包代码的 python3 实现（hashlib/hmac）按 ACS3-HMAC-SHA256
// 规范直接计算：canonical request = METHOD \n canonicalURI \n canonicalQuery \n
// canonicalHeaders（每行 "k:v\n"）\n signedHeaders \n hex(sha256(body))，
// stringToSign = "ACS3-HMAC-SHA256\n" + hex(sha256(canonical request))，
// signature = hex(HMAC-SHA256(AccessKeySecret, stringToSign))。
// percent-encoding 遵循 RFC3986：空格→%20、冒号→%3A、字面加号→%2B、波浪号原样保留。
// 生产发送始终提交空 body，因此每组向量的 x-acs-content-sha256 与 canonical 末行
// 均锁定空载荷哈希 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855。
const goldenPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type acs3GoldenVector struct {
	name         string
	query        url.Values
	when         time.Time
	nonce        string
	wantRawQuery string
	wantDate     string
	wantAuth     string
}

func TestAliyunAcs3SignatureGoldenVectors(t *testing.T) {
	a, err := NewAliyun("AKIDtest0000000000", "testsecret0000000000", "测试签名")
	if err != nil {
		t.Fatal(err)
	}
	for _, vector := range []acs3GoldenVector{
		{
			name: "baseline ascii query with json template param",
			query: url.Values{
				"PhoneNumbers":  {"13800000000"},
				"SignName":      {"PinvouSign"},
				"TemplateCode":  {"SMS_100"},
				"TemplateParam": {`{"code":"654321"}`},
			},
			when:         time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
			nonce:        "27b0d7d4-7d18-4c8e-9d3a-1f2e3d4c5b6a",
			wantRawQuery: "PhoneNumbers=13800000000&SignName=PinvouSign&TemplateCode=SMS_100&TemplateParam=%7B%22code%22%3A%22654321%22%7D",
			wantDate:     "2026-01-02T03:04:05Z",
			wantAuth:     "ACS3-HMAC-SHA256 Credential=AKIDtest0000000000,SignedHeaders=host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version,Signature=1781c6082e0aed0a1edc3e6e5b2533198e8b452705732f79addc3bc4e7f82f69",
		},
		{
			name: "rfc3986 encoding covers space colon tilde literal plus and utf-8",
			query: url.Values{
				"PhoneNumbers":  {"+8613800000000"},
				"SignName":      {"测试签名 Demo~"},
				"TemplateCode":  {"SMS_100"},
				"TemplateParam": {`{"code":"12:34"}`},
			},
			// 非零小数秒落在同一秒内：发送实现按 UTC 整秒归一化（格式化截断
			// 小数部分），wantDate/wantAuth 因此与整秒输入同值——真机时钟常带
			// 小数秒，这里钉住归一化行为。
			when:         time.Date(2026, time.June, 7, 8, 9, 10, 700000000, time.UTC),
			nonce:        "01234567-89ab-cdef-0123-456789abcdef",
			wantRawQuery: "PhoneNumbers=%2B8613800000000&SignName=%E6%B5%8B%E8%AF%95%E7%AD%BE%E5%90%8D%20Demo~&TemplateCode=SMS_100&TemplateParam=%7B%22code%22%3A%2212%3A34%22%7D",
			wantDate:     "2026-06-07T08:09:10Z",
			wantAuth:     "ACS3-HMAC-SHA256 Credential=AKIDtest0000000000,SignedHeaders=host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version,Signature=e56833f157b5fdb77fac15b48cb5f5476b1d212cfa03dc6f71c98414bde27133",
		},
	} {
		t.Run(vector.name, func(t *testing.T) {
			req, err := a.signedRequest(context.Background(), vector.query, vector.when, vector.nonce)
			if err != nil {
				t.Fatal(err)
			}
			if req.Method != http.MethodPost || req.URL.Scheme != "https" || req.URL.Host != smsHost || req.URL.Path != "/" {
				t.Fatalf("unexpected endpoint %s", req.URL)
			}
			if got := req.Header.Get("X-Acs-Content-Sha256"); got != goldenPayloadHash {
				t.Fatalf("x-acs-content-sha256 mismatch: %q", got)
			}
			// 摘要锁定的是零字节载荷：请求体必须实际携带零字节（实现现状是
			// nil body，NoBody 同为空）；任何非空载荷都与 x-acs-content-sha256
			// 不一致，必须红。
			if req.Body != nil {
				sent, readErr := io.ReadAll(req.Body)
				if readErr != nil {
					t.Fatalf("reading the signed request body: %v", readErr)
				}
				if len(sent) != 0 {
					t.Fatalf("signed request must carry a zero-byte payload, got %d bytes", len(sent))
				}
			}
			if req.ContentLength != 0 {
				t.Fatalf("content length must be 0 for an empty payload, got %d", req.ContentLength)
			}
			if got := req.URL.RawQuery; got != vector.wantRawQuery {
				t.Fatalf("canonical query mismatch\n got: %s\nwant: %s", got, vector.wantRawQuery)
			}
			if got := req.Header.Get("X-Acs-Date"); got != vector.wantDate {
				t.Fatalf("x-acs-date mismatch\n got: %s\nwant: %s", got, vector.wantDate)
			}
			if got := req.Header.Get("X-Acs-Signature-Nonce"); got != vector.nonce {
				t.Fatalf("x-acs-signature-nonce must carry the signed nonce\n got: %q\nwant: %q", got, vector.nonce)
			}
			if got := req.Header.Get("Authorization"); got != vector.wantAuth {
				t.Fatalf("authorization mismatch\n got: %s\nwant: %s", got, vector.wantAuth)
			}
		})
	}
}

func TestAliyunAcs3SignatureDifferentialControls(t *testing.T) {
	a, err := NewAliyun("AKIDtest0000000000", "testsecret0000000000", "测试签名")
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	nonce := "27b0d7d4-7d18-4c8e-9d3a-1f2e3d4c5b6a"
	baselineQuery := url.Values{
		"PhoneNumbers":  {"13800000000"},
		"SignName":      {"PinvouSign"},
		"TemplateCode":  {"SMS_100"},
		"TemplateParam": {`{"code":"654321"}`},
	}
	baseline, err := a.signedRequest(context.Background(), baselineQuery, when, nonce)
	if err != nil {
		t.Fatal(err)
	}
	baseAuth := baseline.Header.Get("Authorization")
	if baseAuth != "ACS3-HMAC-SHA256 Credential=AKIDtest0000000000,SignedHeaders=host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version,Signature=1781c6082e0aed0a1edc3e6e5b2533198e8b452705732f79addc3bc4e7f82f69" {
		t.Fatalf("baseline vector drifted: %s", baseAuth)
	}
	// canonical 请求改动一个字节（号码末位 0→1）必须改变签名，次向量同时给出改动后的已知答案。
	oneByteDiff, err := a.signedRequest(context.Background(), url.Values{
		"PhoneNumbers":  {"13800000001"},
		"SignName":      {"PinvouSign"},
		"TemplateCode":  {"SMS_100"},
		"TemplateParam": {`{"code":"654321"}`},
	}, when, nonce)
	if err != nil {
		t.Fatal(err)
	}
	oneByteAuth := oneByteDiff.Header.Get("Authorization")
	if oneByteAuth == baseAuth {
		t.Fatal("a one-byte canonical change did not alter the signature")
	}
	if oneByteAuth != "ACS3-HMAC-SHA256 Credential=AKIDtest0000000000,SignedHeaders=host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version,Signature=3cefba8bc2d43e488feecd04ed7f3e44b5076aba68dc373379a72f6afa4cb723" {
		t.Fatalf("one-byte differential vector mismatch: %s", oneByteAuth)
	}
	// nonce 行必须参与签名：仅更换 nonce 即得到不同签名（次向量为另一已知答案）。
	changedNonce, err := a.signedRequest(context.Background(), baselineQuery, when, "ffffffff-ffff-ffff-ffff-ffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	if got := changedNonce.Header.Get("X-Acs-Signature-Nonce"); got != "ffffffff-ffff-ffff-ffff-ffffffffffff" {
		t.Fatalf("changed nonce must be sent verbatim, got %q", got)
	}
	nonceAuth := changedNonce.Header.Get("Authorization")
	if nonceAuth == baseAuth {
		t.Fatal("changing the nonce did not alter the signature")
	}
	if nonceAuth != "ACS3-HMAC-SHA256 Credential=AKIDtest0000000000,SignedHeaders=host;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version,Signature=a854c31fc953020319a6a0cc5bffa0ff179d6cf041f7cf2fa4ee903219a5dc84" {
		t.Fatalf("nonce differential vector mismatch: %s", nonceAuth)
	}
}
