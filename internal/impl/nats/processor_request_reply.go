package nats

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/benthosdev/benthos/v4/internal/impl/nats/auth"
	"github.com/benthosdev/benthos/v4/public/service"
)

func natsRequestReplyConfig() *service.ConfigSpec {
	return service.NewConfigSpec().
		Categories("Services").
		Version("4.24.0").
		Summary("Sends a message to a NATS subject and expects a reply, from a NATS subscriber acting as a responder, back.").
		Description(`
### Metadata

This input adds the following metadata fields to each message:

` + "```text" + `
- nats_subject
- nats_sequence_stream
- nats_sequence_consumer
- nats_num_delivered
- nats_num_pending
- nats_domain
- nats_timestamp_unix_nano
` + "```" + `

You can access these metadata fields using
[function interpolation](/docs/configuration/interpolation#bloblang-queries).

` + ConnectionNameDescription() + auth.Description()).
		Field(service.NewStringListField("urls").
			Description("A list of URLs to connect to. If an item of the list contains commas it will be expanded into multiple URLs.").
			Example([]string{"nats://127.0.0.1:4222"}).
			Example([]string{"nats://username:password@127.0.0.1:4222"})).
		Field(service.NewInterpolatedStringField("subject").
			Description("A subject to write to.").
			Example("foo.bar.baz").
			Example(`${! meta("kafka_topic") }`).
			Example(`foo.${! json("meta.type") }`)).
		Field(service.NewStringField("inbox_prefix").
			Description("Set an explicit inbox prefix for the response subject").
			Optional().
			Advanced().
			Example("_INBOX_joe")).
		Field(service.NewInterpolatedStringMapField("headers").
			Description("Explicit message headers to add to messages.").
			Default(map[string]any{}).
			Example(map[string]any{
				"Content-Type": "application/json",
				"Timestamp":    `${!meta("Timestamp")}`,
			})).
		Field(service.NewMetadataFilterField("metadata").
			Description("Determine which (if any) metadata values should be added to messages as headers.").
			Optional()).
		Field(service.NewStringField("timeout").
			Description("A duration string is a possibly signed sequence of decimal numbers, each with optional fraction and a unit suffix, such as 300ms, -1.5h or 2h45m. Valid time units are ns, us (or µs), ms, s, m, h.").
			Optional().
			Default("3s")).
		Field(service.NewTLSToggledField("tls")).
		Field(service.NewInternalField(auth.FieldSpec()))
}

func init() {
	err := service.RegisterProcessor("nats_request_reply", natsRequestReplyConfig(), newRequestReplyProcessor)
	if err != nil {
		panic(err)
	}
}

type requestReplyProcessor struct {
	label       string
	urls        string
	headers     map[string]*service.InterpolatedString
	metaFilter  *service.MetadataFilter
	subject     *service.InterpolatedString
	inboxPrefix string
	timeout     time.Duration
	tlsConf     *tls.Config
	authConf    auth.Config

	log *service.Logger
	fs  *service.FS

	natsConn *nats.Conn
	connMut  sync.RWMutex
}

func newRequestReplyProcessor(conf *service.ParsedConfig, mgr *service.Resources) (service.Processor, error) {
	p := &requestReplyProcessor{
		label: mgr.Label(),
		log:   mgr.Logger(),
		fs:    mgr.FS(),
	}
	urlList, err := conf.FieldStringList("urls")
	if err != nil {
		return nil, err
	}
	p.urls = strings.Join(urlList, ",")

	if p.subject, err = conf.FieldInterpolatedString("subject"); err != nil {
		return nil, err
	}

	if conf.Contains("inbox_prefix") {
		if p.inboxPrefix, err = conf.FieldString("inbox_prefix"); err != nil {
			return nil, err
		}
	}

	if p.headers, err = conf.FieldInterpolatedStringMap("headers"); err != nil {
		return nil, err
	}
	timeoutStr, err := conf.FieldString("timeout")
	if err != nil {
		return nil, err
	}
	if p.timeout, err = time.ParseDuration(timeoutStr); err != nil {
		return nil, err
	}

	tlsConf, tlsEnabled, err := conf.FieldTLSToggled("tls")
	if err != nil {
		return nil, err
	}
	if tlsEnabled {
		p.tlsConf = tlsConf
	}

	if p.authConf, err = AuthFromParsedConfig(conf.Namespace("auth")); err != nil {
		return nil, err
	}

	if err = p.connect(context.Background()); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *requestReplyProcessor) connect(ctx context.Context) (err error) {
	p.connMut.Lock()
	defer p.connMut.Unlock()

	var opts []nats.Option
	if p.tlsConf != nil {
		opts = append(opts, nats.Secure(p.tlsConf))
	}

	if p.inboxPrefix != "" {
		opts = append(opts, nats.CustomInboxPrefix(p.inboxPrefix))
	}

	opts = append(opts, nats.Name(p.label))
	opts = append(opts, authConfToOptions(p.authConf, p.fs)...)
	opts = append(opts, errorHandlerOption(p.log))
	opts = append(opts, nats.Timeout(p.timeout))

	if p.natsConn, err = nats.Connect(p.urls, opts...); err != nil {
		return err
	}
	return nil
}

func (r *requestReplyProcessor) Process(ctx context.Context, msg *service.Message) (service.MessageBatch, error) {
	r.connMut.RLock()
	defer r.connMut.RUnlock()

	subject, err := r.subject.TryString(msg)
	if err != nil {
		return nil, err
	}

	nMsg := nats.NewMsg(subject)
	nMsg.Data, err = msg.AsBytes()
	if err != nil {
		return nil, err
	}

	if r.natsConn.HeadersSupported() {
		for k, v := range r.headers {
			headerStr, err := v.TryString(msg)
			if err != nil {
				return nil, fmt.Errorf("header %v interpolation error: %w", k, err)
			}
			nMsg.Header.Add(k, headerStr)
		}
		_ = r.metaFilter.Walk(msg, func(key, value string) error {
			nMsg.Header.Add(key, value)
			return nil
		})
	}

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	resp, err := r.natsConn.RequestMsgWithContext(callCtx, nMsg)
	if err != nil {
		return nil, err
	}
	msg, _, err = convertMessage(resp)
	if err != nil {
		return nil, err
	}
	return service.MessageBatch{msg}, nil
}

func (r *requestReplyProcessor) Close(ctx context.Context) error {
	r.connMut.Lock()
	defer r.connMut.Unlock()

	if r.natsConn != nil {
		r.natsConn.Close()
		r.natsConn = nil
	}
	return nil
}