package webrtc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"m1k1o/neko/internal/config"
	"m1k1o/neko/internal/types"
	"m1k1o/neko/internal/types/codec"
	"m1k1o/neko/internal/webrtc/pionlog"
)

func New(sessions types.SessionManager, capture types.CaptureManager, desktop types.DesktopManager, config *config.WebRTC) *WebRTCManager {
	return &WebRTCManager{
		logger:   log.With().Str("module", "webrtc").Logger(),
		capture:  capture,
		desktop:  desktop,
		sessions: sessions,
		config:   config,
	}
}

type WebRTCManager struct {
	logger     zerolog.Logger
	videoTrack *webrtc.TrackLocalStaticSample
	audioTrack *webrtc.TrackLocalStaticSample
	sessions   types.SessionManager
	capture    types.CaptureManager
	desktop    types.DesktopManager
	config     *config.WebRTC
	api        *webrtc.API

	screenshareStop *func()
}

func (manager *WebRTCManager) Start() {
	var err error

	//
	// audio
	//

	audioCodec := manager.capture.Audio().Codec()
	manager.audioTrack, err = webrtc.NewTrackLocalStaticSample(audioCodec.Capability, "audio", "stream")
	if err != nil {
		manager.logger.Panic().Err(err).Msg("unable to create audio track")
	}

	manager.capture.Audio().OnSample(func(sample types.Sample) {
		err := manager.audioTrack.WriteSample(media.Sample(sample))
		if err != nil && errors.Is(err, io.ErrClosedPipe) {
			manager.logger.Warn().Err(err).Msg("audio pipeline failed to write")
		}
	})

	//
	// video
	//

	videoCodec := manager.capture.Video().Codec()
	manager.videoTrack, err = webrtc.NewTrackLocalStaticSample(videoCodec.Capability, "video", "stream")
	if err != nil {
		manager.logger.Panic().Err(err).Msg("unable to create video track")
	}

	lastScreenshareSample := time.Time{}

	manager.capture.Video().OnSample(func(sample types.Sample) {
		// if screenshare is active, we need to drop all video samples
		// ideally we would stop the video capture meanwhile.
		if lastScreenshareSample.Add(1 * time.Second).After(time.Now()) {
			return
		}

		err := manager.videoTrack.WriteSample(media.Sample(sample))
		if err != nil && errors.Is(err, io.ErrClosedPipe) {
			manager.logger.Warn().Err(err).Msg("video pipeline failed to write")
		}
	})

	manager.capture.Screenshare().OnSample(func(sample types.Sample) {
		lastScreenshareSample = time.Now()
		err := manager.videoTrack.WriteSample(media.Sample(sample))
		if err != nil && errors.Is(err, io.ErrClosedPipe) {
			manager.logger.Warn().Err(err).Msg("video pipeline failed to write")
		}
	})

	//
	// api
	//

	if err := manager.initAPI(); err != nil {
		manager.logger.Panic().Err(err).Msg("failed to initialize webrtc API")
	}

	manager.logger.Info().
		Str("ice_lite", fmt.Sprintf("%t", manager.config.ICELite)).
		Str("ice_servers", fmt.Sprintf("%+v", manager.config.ICEServers)).
		Str("ephemeral_port_range", fmt.Sprintf("%d-%d", manager.config.EphemeralMin, manager.config.EphemeralMax)).
		Str("nat_ips", strings.Join(manager.config.NAT1To1IPs, ",")).
		Msgf("webrtc starting")
}

func (manager *WebRTCManager) Shutdown() error {
	manager.logger.Info().Msgf("webrtc shutting down")
	return nil
}

func (manager *WebRTCManager) initAPI() error {
	logger := pionlog.New(manager.logger)

	settings := webrtc.SettingEngine{
		LoggerFactory: logger,
	}

	_ = settings.SetEphemeralUDPPortRange(manager.config.EphemeralMin, manager.config.EphemeralMax)
	settings.SetNAT1To1IPs(manager.config.NAT1To1IPs, webrtc.ICECandidateTypeHost)
	settings.SetICETimeouts(6*time.Second, 6*time.Second, 3*time.Second)
	settings.SetSRTPReplayProtectionWindow(512)
	settings.SetLite(manager.config.ICELite)

	var networkType []webrtc.NetworkType

	// Add TCP Mux
	if manager.config.TCPMUX > 0 {
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
			IP:   net.IP{0, 0, 0, 0},
			Port: manager.config.TCPMUX,
		})

		if err != nil {
			return err
		}

		tcpMux := webrtc.NewICETCPMux(logger.NewLogger("ice-tcp"), tcpListener, 32)
		settings.SetICETCPMux(tcpMux)

		networkType = append(networkType, webrtc.NetworkTypeTCP4)
		manager.logger.Info().Str("listener", tcpListener.Addr().String()).Msg("using TCP MUX")
	}

	// Add UDP Mux
	if manager.config.UDPMUX > 0 {
		udpListener, err := net.ListenUDP("udp", &net.UDPAddr{
			IP:   net.IP{0, 0, 0, 0},
			Port: manager.config.UDPMUX,
		})

		if err != nil {
			return err
		}

		udpMux := webrtc.NewICEUDPMux(logger.NewLogger("ice-udp"), udpListener)
		settings.SetICEUDPMux(udpMux)

		networkType = append(networkType, webrtc.NetworkTypeUDP4)
		manager.logger.Info().Str("listener", udpListener.LocalAddr().String()).Msg("using UDP MUX")
	}

	// Enable support for TCP and UDP ICE candidates
	if len(networkType) > 0 {
		settings.SetNetworkTypes(networkType)
	}

	// Create MediaEngine with selected codecs
	engine := webrtc.MediaEngine{}
	manager.capture.Audio().Codec().Register(&engine)
	manager.capture.Video().Codec().Register(&engine)

	// Register Interceptors
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(&engine, i); err != nil {
		return err
	}

	// Create API with MediaEngine and SettingEngine
	manager.api = webrtc.NewAPI(
		webrtc.WithMediaEngine(&engine),
		webrtc.WithSettingEngine(settings),
		webrtc.WithInterceptorRegistry(i),
	)

	return nil
}

func (manager *WebRTCManager) CreatePeer(id string, session types.Session) (types.Peer, error) {
	configuration := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	if !manager.config.ICELite {
		configuration.ICEServers = manager.config.ICEServers
	}

	// Create new peer connection
	connection, err := manager.api.NewPeerConnection(configuration)
	if err != nil {
		return nil, err
	}

	negotiated := true
	_, err = connection.CreateDataChannel("data", &webrtc.DataChannelInit{
		Negotiated: &negotiated,
	})
	if err != nil {
		return nil, err
	}

	connection.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if err = manager.handle(id, msg); err != nil {
				manager.logger.Warn().Err(err).Msg("data handle failed")
			}
		})
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	connection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		manager.logger.Info().
			Str("connection_state", connectionState.String()).
			Msg("connection state has changed")
	})

	rtpVideo, err := connection.AddTrack(manager.videoTrack)
	if err != nil {
		return nil, err
	}

	rtpAudio, err := connection.AddTrack(manager.audioTrack)
	if err != nil {
		return nil, err
	}

	connection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected:
			manager.logger.Info().Str("id", id).Msg("peer disconnected")
			manager.sessions.Destroy(id)
		case webrtc.PeerConnectionStateFailed:
			manager.logger.Warn().Str("id", id).Msg("peer failed")
			manager.sessions.Destroy(id)
		case webrtc.PeerConnectionStateClosed:
			manager.logger.Info().Str("id", id).Msg("peer closed")
			manager.sessions.Destroy(id)
		case webrtc.PeerConnectionStateConnected:
			manager.logger.Info().Str("id", id).Msg("peer connected")
			if err = session.SetConnected(true); err != nil {
				manager.logger.Warn().Err(err).Msg("unable to set connected on peer")
				manager.sessions.Destroy(id)
			}
		}
	})

	peer := &Peer{
		id:         id,
		manager:    manager,
		connection: connection,
	}

	connection.OnNegotiationNeeded(func() {
		manager.logger.Warn().Msg("negotiation is needed")

		sdp, err := peer.CreateOffer()
		if err != nil {
			manager.logger.Err(err).Msg("creating offer failed")
			return
		}

		err = session.SignalLocalOffer(sdp)
		if err != nil {
			manager.logger.Warn().Err(err).Msg("sending SignalLocalOffer failed")
			return
		}
	})

	connection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			manager.logger.Info().Msg("sent all ICECandidates")
			return
		}

		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			manager.logger.Warn().Err(err).Msg("converting ICECandidate to json failed")
			return
		}

		if err := session.SignalCandidate(string(candidateString)); err != nil {
			manager.logger.Warn().Err(err).Msg("sending SignalCandidate failed")
			return
		}
	})

	connection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		logger := manager.logger.With().
			Str("kind", track.Kind().String()).
			Str("mime", track.Codec().RTPCodecCapability.MimeType).
			Logger()

		logger.Info().Msgf("received new remote track")

		// parse codec from remote track
		codec, ok := codec.ParseRTC(track.Codec())
		if !ok {
			logger.Warn().Msg("remote track with unknown codec")
			receiver.Stop()
			return
		}

		var srcSinkManager types.StreamSrcSinkManager

		stopped := false
		stopFn := func() {
			if stopped {
				return
			}

			stopped = true
			receiver.Stop()
			srcSinkManager.Stop()
			logger.Info().Msg("remote track stopped")
		}

		logger.Info().Msgf("found codec %s", codec.Name)

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			// video -> webcam
			srcSinkManager = manager.capture.Screenshare()
			defer stopFn()

			if manager.screenshareStop != nil {
				(*manager.screenshareStop)()
			}
			manager.screenshareStop = &stopFn
		} else {
			logger.Warn().Msg("expected only video tracks")
			receiver.Stop()
			return
		}

		err := srcSinkManager.Start(codec)
		if err != nil {
			logger.Err(err).Msg("failed to start pipeline")
			return
		}

		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		go func() {
			for range ticker.C {
				err := connection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
				if err != nil {
					logger.Err(err).Msg("remote track rtcp send err")
				}
			}
		}()

		buf := make([]byte, 1400)
		for {
			i, _, err := track.Read(buf)
			if err != nil {
				logger.Warn().Err(err).Msg("failed read from remote track")
				break
			}

			srcSinkManager.Push(buf[:i])
		}
	})

	if err := session.SetPeer(peer); err != nil {
		return nil, err
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpVideo.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpAudio.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	return peer, nil
}

func (manager *WebRTCManager) ICELite() bool {
	return manager.config.ICELite
}

func (manager *WebRTCManager) ICEServers() []webrtc.ICEServer {
	return manager.config.ICEServers
}

func (manager *WebRTCManager) ImplicitControl() bool {
	return manager.config.ImplicitControl
}
