package events

import (
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/nats-io/nats.go/jetstream"
)

func TestAuthorizationConsumerResourceContract(t *testing.T) {
	descriptors := append(approvalconsumer.All(), approvalconsumer.LegacyBilling())
	for _, descriptor := range descriptors {
		t.Run(descriptor.Durable, func(t *testing.T) {
			var start uint64
			if descriptor.StartSource == approvalconsumer.StartQuiescingHighWatermarkPlusOne {
				start = 43
				if _, err := authorizationConsumerConfig(descriptor, 0); !errors.Is(err, ErrNATSResources) {
					t.Fatal("new Billing durable accepted missing Q+1")
				}
			} else if _, err := authorizationConsumerConfig(descriptor, 43); !errors.Is(err, ErrNATSResources) {
				t.Fatal("existing durable accepted replacement start")
			}
			config, err := authorizationConsumerConfig(descriptor, start)
			if err != nil || config.AckWait != 30*time.Second || len(config.BackOff) != 0 || config.MaxDeliver != -1 || config.MaxAckPending != 64 || config.FilterSubject != authorizationSubject {
				t.Fatalf("invalid dedicated config: %+v, %v", config, err)
			}
			if start != 0 && (config.DeliverPolicy != jetstream.DeliverByStartSequencePolicy || config.OptStartSeq != start) {
				t.Fatal("Q+1 not preserved")
			}
			if !validAuthorizationConsumerConfig(config, config) {
				t.Fatal("exact resource rejected")
			}
			mutations := map[string]func(*jetstream.ConsumerConfig){
				"ack-wait":       func(c *jetstream.ConsumerConfig) { c.AckWait = time.Second },
				"backoff":        func(c *jetstream.ConsumerConfig) { c.BackOff = []time.Duration{30 * time.Second} },
				"delivery-limit": func(c *jetstream.ConsumerConfig) { c.MaxDeliver = 12 },
				"pending-limit":  func(c *jetstream.ConsumerConfig) { c.MaxAckPending = 65 },
				"subject":        func(c *jetstream.ConsumerConfig) { c.FilterSubject = "account.>" },
				"start":          func(c *jetstream.ConsumerConfig) { c.OptStartSeq++ },
			}
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					changed := config
					mutate(&changed)
					if validAuthorizationConsumerConfig(changed, config) {
						t.Fatal("resource drift accepted")
					}
				})
			}
			forged := descriptor
			forged.Workload = "other-workload"
			if _, err := authorizationConsumerConfig(forged, start); !errors.Is(err, ErrNATSResources) {
				t.Fatal("caller-defined descriptor accepted")
			}
		})
	}
}
