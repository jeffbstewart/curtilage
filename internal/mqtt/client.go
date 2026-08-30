// Package mqtt subscribes to the broker and turns every message into a
// record.Record on a channel (docs/DESIGN.md "Ingest: MQTT directly").
//
// Run owns the channel's lifetime: it closes out when it returns, so a
// downstream record.Write sees a clean end of stream on shutdown and
// finishes its MCAP file.  Availability is published retained under
// <prefix>/available with an "offline" last will.
package mqtt

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"google.golang.org/protobuf/types/known/timestamppb"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
	"github.com/jeffbstewart/curtilage/internal/record"
)

// Options for one connection.
type Options struct {
	Host          string
	Port          uint32
	ClientID      string
	User          string
	Password      string
	Keepalive     time.Duration
	Subscriptions []string
	PublishPrefix string
}

// Counters and gauges for /metrics.
var (
	connected  atomic.Bool
	reconnects atomic.Uint64
	dropped    atomic.Uint64 // out was full: the recorder is not keeping up
	byPrefix   sync.Map      // topic first segment -> *atomic.Uint64
)

// Stats returns the counters for /metrics.  messages is keyed by the
// topic's first path segment (bounded cardinality).
func Stats() (isConnected bool, reconnectCount, droppedCount uint64, messages map[string]uint64) {
	messages = map[string]uint64{}
	byPrefix.Range(func(k, v any) bool {
		messages[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return connected.Load(), reconnects.Load(), dropped.Load(), messages
}

func count(topic string) {
	seg := topic
	if i := strings.IndexByte(topic, '/'); i >= 0 {
		seg = topic[:i]
	}
	c, _ := byPrefix.LoadOrStore(seg, &atomic.Uint64{})
	c.(*atomic.Uint64).Add(1)
}

// Run connects, subscribes, and forwards messages to out until ctx is
// cancelled; then it publishes "offline", disconnects, and CLOSES out.
// A full out drops the message and counts it rather than stalling the
// broker session.
func Run(ctx context.Context, o Options, out chan<- *record.Record) error {
	defer close(out)
	if o.Port == 0 {
		o.Port = 1883
	}
	if o.Keepalive == 0 {
		o.Keepalive = 30 * time.Second
	}
	if o.PublishPrefix == "" {
		o.PublishPrefix = "curtilage"
	}
	avail := o.PublishPrefix + "/available"

	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", o.Host, o.Port)).
		SetClientID(o.ClientID).
		SetKeepAlive(o.Keepalive).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5*time.Second).
		SetOrderMatters(false).
		SetWill(avail, "offline", 1, true)
	if o.User != "" {
		opts.SetUsername(o.User).SetPassword(o.Password)
	}
	handler := func(_ paho.Client, m paho.Message) {
		count(m.Topic())
		payload := append([]byte(nil), m.Payload()...)
		rec := &record.Record{
			ReceivedAt:    timestamppb.Now(),
			Topic:         m.Topic(),
			Retained:      m.Retained(),
			Qos:           qosEnum(m.Qos()),
			Payload:       payload,
			PayloadCrc32C: record.Checksum(payload),
		}
		select {
		case out <- rec:
		default:
			dropped.Add(1)
		}
	}
	first := true
	opts.SetOnConnectHandler(func(c paho.Client) {
		connected.Store(true)
		if !first {
			reconnects.Add(1)
		}
		first = false
		filters := map[string]byte{}
		for _, s := range o.Subscriptions {
			filters[s] = 0
		}
		if t := c.SubscribeMultiple(filters, handler); t.Wait() && t.Error() != nil {
			log.Printf("mqtt: subscribe: %v", t.Error())
		}
		if t := c.Publish(avail, 1, true, "online"); t.Wait() && t.Error() != nil {
			log.Printf("mqtt: publish available: %v", t.Error())
		}
		log.Printf("mqtt: connected to %s:%d as %q, subscribed to %v", o.Host, o.Port, o.ClientID, o.Subscriptions)
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		connected.Store(false)
		log.Printf("mqtt: connection lost: %v", err)
	})

	c := paho.NewClient(opts)
	if t := c.Connect(); t.Wait() && t.Error() != nil {
		// With ConnectRetry the token only errors on a permanent
		// refusal (bad credentials); transient failures keep retrying.
		return fmt.Errorf("mqtt: connect: %w", t.Error())
	}
	<-ctx.Done()
	if c.IsConnectionOpen() {
		if t := c.Publish(avail, 1, true, "offline"); !t.WaitTimeout(2 * time.Second) {
			log.Print("mqtt: offline publish timed out")
		}
	}
	c.Disconnect(500) // ms: lets the offline publish flush
	connected.Store(false)
	return nil
}

// qosEnum maps the wire QoS (0..2) to the schema's named levels.
func qosEnum(q byte) curtilagev1.Qos {
	switch q {
	case 0:
		return curtilagev1.Qos_QOS_AT_MOST_ONCE
	case 1:
		return curtilagev1.Qos_QOS_AT_LEAST_ONCE
	case 2:
		return curtilagev1.Qos_QOS_EXACTLY_ONCE
	}
	return curtilagev1.Qos_QOS_UNSPECIFIED
}
