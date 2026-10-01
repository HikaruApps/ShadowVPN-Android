//go:build android

package bridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/sagernet/cronet-go"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xrayerrors "github.com/xtls/xray-core/common/errors"
	xraynet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	featureoutbound "github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport"
)

// naiveRuntime owns the Cronet-backed Naive transport. Traffic reaches it
// directly through naiveOutboundHandler: there is no localhost SOCKS bridge.
type naiveRuntime struct {
	client    *cronet.NaiveClient
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func newNaiveRuntime(profile Profile) (*naiveRuntime, error) {
	settings, _ := profile.outbound["settings"].(map[string]any)
	if settings == nil {
		return nil, errors.New("Naive profile has no settings")
	}
	address := strings.TrimSpace(stringValue(settings["address"]))
	serverName := strings.TrimSpace(stringValue(settings["serverName"]))
	if serverName == "" {
		serverName = address
	}
	port := integerValue(settings["port"])
	if address == "" || port < 1 || port > 65535 {
		return nil, errors.New("Naive profile has an invalid server endpoint")
	}

	congestion, err := naiveCongestionControl(stringValue(settings["congestionControl"]))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	client, err := cronet.NewNaiveClient(cronet.NaiveClientOptions{
		Context:               ctx,
		ServerAddress:         M.ParseSocksaddrHostPort(address, uint16(port)),
		ServerName:            serverName,
		Username:              stringValue(settings["username"]),
		Password:              stringValue(settings["password"]),
		InsecureConcurrency:   integerValue(settings["insecureConcurrency"]),
		DNSResolver:           naiveDNSResolver,
		QUIC:                  boolValue(settings["quic"]),
		QUICCongestionControl: congestion,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Naive/Cronet client: %w", err)
	}
	return &naiveRuntime{client: client, cancel: cancel}, nil
}

func (runtime *naiveRuntime) Start() error {
	if err := runtime.client.Start(); err != nil {
		runtime.Close()
		return fmt.Errorf("failed to start Naive/Cronet client: %w", err)
	}
	return nil
}

// Install swaps the JSON-loadable placeholder for a native Cronet handler
// before Xray starts. Xray still owns TUN/gVisor, routing, DNS and accounting.
func (runtime *naiveRuntime) Install(instance *core.Instance, tag string) error {
	feature := instance.GetFeature(featureoutbound.ManagerType())
	manager, ok := feature.(featureoutbound.Manager)
	if !ok || manager == nil {
		return errors.New("Xray outbound manager is unavailable")
	}
	ctx := context.Background()
	if err := manager.RemoveHandler(ctx, tag); err != nil {
		return fmt.Errorf("failed to remove Naive placeholder outbound: %w", err)
	}
	if err := manager.AddHandler(ctx, &naiveOutboundHandler{tag: tag, runtime: runtime}); err != nil {
		return fmt.Errorf("failed to install native Naive outbound: %w", err)
	}
	return nil
}

func (runtime *naiveRuntime) Close() error {
	var closeErr error
	runtime.closeOnce.Do(func() {
		runtime.cancel()
		closeErr = runtime.client.Close()
	})
	return closeErr
}

type naiveOutboundHandler struct {
	tag     string
	runtime *naiveRuntime
}

func (handler *naiveOutboundHandler) Start() error { return nil }
func (handler *naiveOutboundHandler) Close() error { return nil }
func (handler *naiveOutboundHandler) Tag() string  { return handler.tag }

func (handler *naiveOutboundHandler) SenderSettings() *serial.TypedMessage { return nil }
func (handler *naiveOutboundHandler) ProxySettings() *serial.TypedMessage  { return nil }

func (handler *naiveOutboundHandler) Dispatch(ctx context.Context, link *transport.Link) {
	err := handler.process(ctx, link)
	if err != nil {
		wrapped := xrayerrors.New("failed to process native Naive outbound traffic").Base(err)
		session.SubmitOutboundErrorToOriginator(ctx, wrapped)
		xrayerrors.LogInfo(ctx, wrapped.Error())
		common.Interrupt(link.Writer)
	} else {
		common.Close(link.Writer)
	}
	common.Interrupt(link.Reader)
}

func (handler *naiveOutboundHandler) process(ctx context.Context, link *transport.Link) error {
	outbounds := session.OutboundsFromContext(ctx)
	if len(outbounds) == 0 || !outbounds[len(outbounds)-1].Target.IsValid() {
		return errors.New("Naive target is not specified")
	}
	destination := outbounds[len(outbounds)-1].Target
	if destination.Network != xraynet.Network_TCP {
		return errors.New("Naive/Cronet UDP is not supported by this core")
	}

	target := M.ParseSocksaddrHostPort(destination.Address.String(), uint16(destination.Port))
	connection, err := handler.runtime.client.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("failed to dial Naive target %s: %w", destination.String(), err)
	}
	defer connection.Close()

	requestDone := func() error {
		return buf.Copy(link.Reader, buf.NewWriter(connection))
	}
	responseDone := func() error {
		return buf.Copy(buf.NewReader(connection), link.Writer)
	}
	return task.Run(ctx, requestDone, task.OnSuccess(responseDone, task.Close(link.Writer)))
}

func naiveDNSResolver(ctx context.Context, request *dns.Msg) *dns.Msg {
	client := &dns.Client{Net: "udp", Timeout: 4 * time.Second}
	for _, server := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		response, _, err := client.ExchangeContext(ctx, request.Copy(), server)
		if err == nil && response != nil {
			return response
		}
	}
	response := new(dns.Msg)
	response.SetRcode(request, dns.RcodeServerFailure)
	return response
}

func naiveCongestionControl(value string) (cronet.QUICCongestionControl, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return cronet.QUICCongestionControlDefault, nil
	case "bbr":
		return cronet.QUICCongestionControlBBR, nil
	case "bbr2":
		return cronet.QUICCongestionControlBBRv2, nil
	case "cubic":
		return cronet.QUICCongestionControlCubic, nil
	case "reno":
		return cronet.QUICCongestionControlReno, nil
	default:
		return "", fmt.Errorf("unknown Naive congestion-control: %s", strconv.Quote(value))
	}
}

func boolValue(value any) bool {
	switch value := value.(type) {
	case bool:
		return value
	case string:
		return value == "1" || strings.EqualFold(value, "true")
	default:
		return false
	}
}
