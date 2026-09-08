package events

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestNATSPrivateCARootsRejectUnsafeInputs(t *testing.T) {
	ca, leaf, key := natsFixtureTLS(t)
	root := t.TempDir()
	for _, item := range []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"valid", ca, true}, {"empty", nil, false}, {"garbage", []byte("not a certificate"), false},
		{"leaf", leaf, false}, {"key", key, false},
		{"prefix", append([]byte("garbage\n"), ca...), false},
		{"suffix", append(append([]byte{}, ca...), key...), false},
		{"oversized", make([]byte, (1<<20)+1), false},
	} {
		file := filepath.Join(root, item.name)
		if os.WriteFile(file, item.body, 0600) != nil {
			t.Fatal("CA fixture write failed")
		}
		_, err := natsRoots(file)
		if item.valid != (err == nil) {
			t.Fatalf("CA reader wrong result: %s", item.name)
		}
	}
	if _, err := natsRoots("relative.crt"); err == nil {
		t.Fatal("relative CA path accepted")
	}
	if runtime.GOOS != "windows" {
		file := filepath.Join(root, "valid")
		if os.Chmod(file, 0666) != nil {
			t.Fatal("CA mode setup failed")
		}
		if _, err := natsRoots(file); err == nil {
			t.Fatal("writable CA accepted")
		}
		os.Chmod(file, 0600)
		link := filepath.Join(root, "linked")
		if os.Symlink(file, link) != nil {
			t.Fatal("CA symlink setup failed")
		}
		if _, err := natsRoots(link); err == nil {
			t.Fatal("symlink CA accepted")
		}
	}
}

func TestNATSResourceModeCannotDowngradeProduction(t *testing.T) {
	for _, environment := range []string{"production", "production-new", " PRODUCTION-uat "} {
		for _, mode := range []string{"", "provisioned", "auto", "unknown"} {
			provisioned, err := (NATSConfig{Environment: environment, ResourceMode: mode}).provisioned()
			allowed := mode == "" || mode == "provisioned"
			if allowed != (err == nil) || (allowed && !provisioned) {
				t.Fatal("production provisioning mode bypass")
			}
		}
	}
	if provisioned, err := (NATSConfig{Environment: "development"}).provisioned(); err != nil || provisioned {
		t.Fatal("development behavior changed")
	}
}

func TestNATSFixedSpecificationsRejectDrift(t *testing.T) {
	if !validStreamConfig(platformStreamConfig()) {
		t.Fatal("canonical stream rejected")
	}
	metadata := platformStreamConfig()
	metadata.Metadata = map[string]string{"_nats.level": "1", "_nats.req.level": "0", "_nats.ver": "2.11.17"}
	if !validStreamConfig(metadata) {
		t.Fatal("documented server metadata rejected")
	}
	metadata.Metadata["unreviewed"] = "change"
	if validStreamConfig(metadata) {
		t.Fatal("custom metadata drift ignored")
	}
	for _, mutate := range []func(*jetstream.StreamConfig){
		func(c *jetstream.StreamConfig) { c.Subjects = append(c.Subjects, "foreign.>") },
		func(c *jetstream.StreamConfig) { c.Storage = jetstream.MemoryStorage },
		func(c *jetstream.StreamConfig) { c.Retention = jetstream.WorkQueuePolicy },
		func(c *jetstream.StreamConfig) { c.MaxAge = time.Hour },
		func(c *jetstream.StreamConfig) { c.Duplicates = time.Minute },
		func(c *jetstream.StreamConfig) { c.AllowRollup = true },
		func(c *jetstream.StreamConfig) { c.AllowDirect = true },
		func(c *jetstream.StreamConfig) { c.NoAck = true },
		func(c *jetstream.StreamConfig) { c.Mirror = &jetstream.StreamSource{Name: "foreign"} },
		func(c *jetstream.StreamConfig) { c.Sources = []*jetstream.StreamSource{{Name: "foreign"}} },
		func(c *jetstream.StreamConfig) {
			c.SubjectTransform = &jetstream.SubjectTransformConfig{Source: ">", Destination: "foreign.>"}
		},
		func(c *jetstream.StreamConfig) {
			c.RePublish = &jetstream.RePublish{Source: ">", Destination: "foreign.>"}
		},
		func(c *jetstream.StreamConfig) { c.AllowMsgTTL = true },
		func(c *jetstream.StreamConfig) { c.ConsumerLimits.InactiveThreshold = time.Second },
	} {
		c := platformStreamConfig()
		mutate(&c)
		if validStreamConfig(c) {
			t.Fatal("stream drift accepted")
		}
	}
	for _, expected := range provisionedConsumers() {
		if !validConsumerConfig(expected, expected) {
			t.Fatal("canonical consumer rejected")
		}
		for _, mutate := range []func(*jetstream.ConsumerConfig){
			func(c *jetstream.ConsumerConfig) { c.FilterSubject = "foreign.>" },
			func(c *jetstream.ConsumerConfig) { c.FilterSubjects = []string{"foreign.>"} },
			func(c *jetstream.ConsumerConfig) { c.AckPolicy = jetstream.AckNonePolicy },
			func(c *jetstream.ConsumerConfig) { c.AckWait = time.Minute },
			func(c *jetstream.ConsumerConfig) { c.MaxDeliver = 1 },
			func(c *jetstream.ConsumerConfig) { c.BackOff = nil },
			func(c *jetstream.ConsumerConfig) { c.HeadersOnly = true },
			func(c *jetstream.ConsumerConfig) { c.OptStartSeq = 12 },
			func(c *jetstream.ConsumerConfig) { now := time.Now(); c.OptStartTime = &now },
			func(c *jetstream.ConsumerConfig) { now := time.Now(); c.PauseUntil = &now },
			func(c *jetstream.ConsumerConfig) { c.InactiveThreshold = time.Minute },
			func(c *jetstream.ConsumerConfig) { c.DeliverSubject = "foreign" },
			func(c *jetstream.ConsumerConfig) { c.DeliverGroup = "foreign" },
			func(c *jetstream.ConsumerConfig) { c.MemoryStorage = true },
		} {
			c := expected
			mutate(&c)
			if validConsumerConfig(c, expected) {
				t.Fatal("consumer drift accepted")
			}
		}
		for _, role := range []string{"account-outbox-relay", "billing-outbox-relay", "token-outbox-relay", "unknown"} {
			if consumerOwnedBy(role, expected) {
				t.Fatal("relay gained consumer capability")
			}
		}
	}
}
