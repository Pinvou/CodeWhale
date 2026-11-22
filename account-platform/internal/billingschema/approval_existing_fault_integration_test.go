package billingschema

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

type existingBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c existingBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// Decode only protocol framing, never log credentials or payloads. This owned
// TLS transport proxy can discard an actual server success reply after the
// server has already changed its durable, rather than mocking that mutation.
func existingProtocolFrame(reader *bufio.Reader) ([]byte, []byte, []string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, nil, nil, err
	}
	if len(line) > 64<<10 {
		return nil, nil, nil, errors.New("owned frame limit")
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return []byte(line), nil, fields, nil
	}
	payloadFrame := fields[0] == "PUB" || fields[0] == "HPUB" || fields[0] == "MSG" || fields[0] == "HMSG"
	if !payloadFrame {
		return []byte(line), nil, fields, nil
	}
	if len(fields) < 3 {
		return nil, nil, nil, errors.New("owned frame shape")
	}
	size, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || size < 0 || size > 256<<10 {
		return nil, nil, nil, errors.New("owned payload limit")
	}
	body := make([]byte, size+2)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, nil, nil, err
	}
	if !bytes.Equal(body[size:], []byte("\r\n")) {
		return nil, nil, nil, errors.New("owned payload terminator")
	}
	payload := body[:size]
	if fields[0] == "HMSG" || fields[0] == "HPUB" {
		headerSize, err := strconv.Atoi(fields[len(fields)-2])
		if err != nil || headerSize < 0 || headerSize > size {
			return nil, nil, nil, errors.New("owned header shape")
		}
		payload = payload[headerSize:]
	}
	return append([]byte(line), body...), payload, fields, nil
}

func existingDropConsumerReply(t *testing.T, ctx context.Context, b existingActualFixture, target string, holdConnection bool) (string, <-chan struct{}, *atomic.Int64) {
	t.Helper()
	upstreamURL, err := url.Parse(b.url)
	if err != nil {
		t.Fatal("owned upstream URL failed")
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Join(filepath.Dir(b.cfg.CAFile), "server.crt"), filepath.Join(filepath.Dir(b.cfg.CAFile), "server.key"))
	if err != nil {
		t.Fatal("owned proxy certificate failed")
	}
	ca, err := os.ReadFile(b.cfg.CAFile)
	if err != nil {
		t.Fatal("owned proxy CA failed")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("owned proxy CA failed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("owned proxy listener failed")
	}
	dropped, requests := make(chan struct{}), &atomic.Int64{}
	var once atomic.Bool
	var mu sync.Mutex
	var wait sync.WaitGroup
	connections := []net.Conn{}
	track := func(c net.Conn) { mu.Lock(); connections = append(connections, c); mu.Unlock() }
	wait.Add(1)
	go func() {
		defer wait.Done()
		for {
			front, err := listener.Accept()
			if err != nil {
				return
			}
			track(front)
			wait.Add(1)
			go func() {
				defer wait.Done()
				defer front.Close()
				back, err := (&net.Dialer{}).DialContext(ctx, "tcp", upstreamURL.Host)
				if err != nil {
					return
				}
				track(back)
				defer back.Close()
				backReader := bufio.NewReader(back)
				info, err := backReader.ReadString('\n')
				if err != nil || !strings.HasPrefix(info, "INFO ") || len(info) > 64<<10 {
					return
				}
				if _, err := io.WriteString(front, info); err != nil {
					return
				}
				upstream := tls.Client(existingBufferedConn{back, backReader}, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: upstreamURL.Hostname()})
				if upstream.HandshakeContext(ctx) != nil {
					return
				}
				downstream := tls.Server(front, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
				if downstream.HandshakeContext(ctx) != nil {
					return
				}
				wait.Add(1)
				go func() {
					defer wait.Done()
					defer back.Close()
					reader := bufio.NewReader(downstream)
					for {
						frame, _, fields, err := existingProtocolFrame(reader)
						if err != nil {
							return
						}
						if len(fields) > 1 && (fields[0] == "PUB" || fields[0] == "HPUB") && (fields[1] == "$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM."+target || strings.HasPrefix(fields[1], "$JS.API.CONSUMER.CREATE.ACCOUNT_PLATFORM."+target+".")) {
							requests.Add(1)
						}
						if _, err := io.Copy(upstream, bytes.NewReader(frame)); err != nil {
							return
						}
					}
				}()
				reader := bufio.NewReader(upstream)
				for {
					frame, payload, _, err := existingProtocolFrame(reader)
					if err != nil {
						return
					}
					var response struct {
						Type string `json:"type"`
						Name string `json:"name"`
					}
					if json.Unmarshal(payload, &response) == nil && strings.HasSuffix(response.Type, ".consumer_create_response") && response.Name == target && once.CompareAndSwap(false, true) {
						close(dropped)
						if holdConnection {
							continue
						}
						return // discard exactly this successful server ACK
					}
					if _, err := io.Copy(downstream, bytes.NewReader(frame)); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		finished := make(chan struct{})
		go func() { wait.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("owned transport cleanup unconfirmed")
		}
	})
	return "tls://" + listener.Addr().String(), dropped, requests
}

func existingFaultCommand(ctx context.Context, binary string, b existingActualFixture, dsn, action string, extra ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, binary, append([]string{"existing-broker", "-action", action, "-expected-version", "3"}, extra...)...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "ACCOUNT_PLATFORM_PROFILE=models", "APPROVAL_INITIALIZER_DATABASE_URL=" + dsn,
		"NATS_URL=" + b.url, "NATS_CA_FILE=" + b.cfg.CAFile, "NATS_NKEY_SEED_FILE=" + b.cfg.SeedFile,
		"APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE=" + b.files.Identity.CurrentFile, "APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_SHA256=" + b.files.Identity.CurrentSHA256,
		"APPROVAL_RESOURCE_MANIFEST_FILE=" + b.files.ManifestFile, "APPROVAL_RESOURCE_MANIFEST_SHA256=" + b.files.ManifestSHA256,
		"APPROVAL_EXISTING_PLAN_FILE=" + b.files.PlanFile, "APPROVAL_EXISTING_PLAN_SHA256=" + b.files.PlanSHA256,
		"APPROVAL_RESOURCE_RECEIPT_FILE=" + b.files.ReceiptFile, "APPROVAL_RESOURCE_RECEIPT_SHA256=" + b.files.ReceiptSHA256}
	return command
}

func TestExistingBrokerCLIUnknownACKReadbackAndRecovery(t *testing.T) {
	raw, natsBinary, binary := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"), os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY"), os.Getenv("PINVOU_TEST_APPROVAL_OPERATOR_BINARY")
	if raw == "" || natsBinary == "" || binary == "" {
		t.Skip("owned PostgreSQL, TLS NATS and compiled operator required")
	}
	for _, scenario := range []struct {
		name                    string
		billing, holdConnection bool
	}{
		{"token-update-interrupted", false, false}, {"billing-create-interrupted", true, false}, {"token-update-timeout", false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			f := newStageFixture(t, ctx, raw)
			db := f.database(t, ctx, "account", "account_service")
			all, err := migrations.Load("../../migrations/account")
			if err != nil || accountschema.Up(ctx, db, all) != nil {
				t.Fatal("actual Account schema failed")
			}
			dsn := f.serviceURL(t, db, "account_migrator")
			b := existingActualBroker(t, ctx, natsBinary)
			facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
			for _, item := range []struct {
				step    account.FreshApprovalStep
				version int64
			}{{account.FreshApprovalQuiesce, 1}, {account.FreshApprovalFence, 2}} {
				if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, item.version, item.step, "", nil); err != nil {
					t.Fatal("owned fence setup failed")
				}
			}
			prepare := func(token bool) {
				t.Helper()
				step := account.ExistingBrokerPrepare
				if token {
					step = account.ExistingBrokerPrepareTokenUpgrade
				}
				_, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, step, "", func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
					body, err := events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString(), token)
					if err == nil {
						b.files.PlanFile = existingPublicWrite(t, b.resources, uuid.NewString()+".json", body)
						b.files.PlanSHA256 = existingPublicHash(body)
					}
					return body, err
				})
				if err != nil {
					t.Fatal("owned committed plan setup failed")
				}
			}
			prepare(true)
			action, inspect, target := "upgrade-token-consumers", "inspect-token-upgrade", "token-model-lifecycle-executor-v1"
			if scenario.billing {
				if _, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 3, account.ExistingBrokerUpgradeToken, b.files.PlanSHA256, func(ctx context.Context, _ account.AdminApprovalControl) (json.RawMessage, error) {
					return events.UpgradeLegacyTokenConsumers(ctx, b.url, b.cfg, b.files, facts, true, false)
				}); err != nil {
					t.Fatal("owned Token transition setup failed")
				}
				prepare(false)
				action, inspect, target = "initialize-consumers", "inspect-consumers", "billing-member-lifecycle-v1"
				b.files.ReceiptFile = filepath.Join(b.resources, "receipt.json")
			}
			originalPlan, err := os.ReadFile(b.files.PlanFile)
			if err != nil {
				t.Fatal("owned plan missing")
			}
			proxy, dropped, requests := existingDropConsumerReply(t, ctx, b, target, scenario.holdConnection)
			throughProxy := b
			throughProxy.url = proxy
			opctx, stop := context.WithCancel(ctx)
			command := existingFaultCommand(opctx, binary, throughProxy, dsn, action)
			if command.Start() != nil {
				stop()
				t.Fatal("owned CLI start failed")
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-dropped:
			case <-time.After(12 * time.Second):
				stop()
				<-done
				t.Fatal("actual server success ACK cutpoint was not reached")
			}
			// A separate scenario keeps the connection alive and observes the real
			// CLI deadline. Neither outcome may automatically resend the mutation.
			startedWaiting := time.Now()
			if !scenario.holdConnection {
				stop()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("lost ACK operation unexpectedly reported success")
				}
				if scenario.holdConnection && time.Since(startedWaiting) < 80*time.Second {
					t.Fatal("CLI did not reach its actual timeout")
				}
			case <-time.After(100 * time.Second):
				stop()
				t.Fatal("owned CLI cancellation unconfirmed")
			}
			stop()
			if requests.Load() != 1 {
				t.Fatal("unknown outcome caused an automatic mutation retry")
			}
			control, err := account.RunExistingBrokerApprovalStep(ctx, dsn, 0, account.FreshApprovalStatus, "", nil)
			if err != nil || control.Version != 3 || control.Mode != account.AdminApprovalModeQuiescing || control.AuthorizationRelayState != account.AdminApprovalRelayFenced {
				t.Fatal("unknown broker outcome escaped maintenance fence")
			}
			var audits int
			if db.QueryRow(ctx, `SELECT count(*) FROM public.audit_logs WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND change_summary->>'step'=$1`, action).Scan(&audits) != nil || audits != 0 {
				t.Fatal("unknown operation was falsely audited as committed")
			}
			if _, err := existingFaultCommand(ctx, binary, b, dsn, inspect).Output(); err != nil {
				t.Fatal("original operation cannot be read back after lost ACK")
			}
			if auditsNow := stageValue(t, ctx, db, `SELECT count(*)::text FROM public.audit_logs WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND change_summary->>'step'='`+action+`'`); auditsNow != "0" {
				t.Fatal("inspection wrote a success audit")
			}
			if _, err := existingFaultCommand(ctx, binary, b, dsn, action, "-resume").Output(); err != nil {
				t.Fatal("explicit original-plan recovery failed")
			}
			currentPlan, err := os.ReadFile(b.files.PlanFile)
			if err != nil || !bytes.Equal(originalPlan, currentPlan) {
				t.Fatal("unknown ACK recovery recaptured its plan")
			}
			stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
			if err != nil {
				t.Fatal("owned stream missing")
			}
			info, err := stream.Info(ctx)
			wantConsumers := 5
			if scenario.billing {
				wantConsumers = 17
			}
			if err != nil || info.State.Msgs != 1 || info.State.LastSeq != 1 || info.State.Consumers != wantConsumers {
				t.Fatal("unknown ACK recovery changed messages or inventory")
			}
		})
	}
}
