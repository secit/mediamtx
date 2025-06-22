package srt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/google/uuid"
	"github.com/haivision/srtgo"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/counterdumper"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/mpegts"
	"github.com/bluenviron/mediamtx/internal/stream"
)

type conn struct {
	parentCtx         context.Context
	rtspAddress       string
	readTimeout       conf.Duration
	writeTimeout      conf.Duration
	udpMaxPayloadSize int
	// connReq           srt.ConnRequest
	connSck struct {
		socket *srtgo.SrtSocket
		addr   *net.UDPAddr
	}
	runOnConnect        string
	runOnConnectRestart bool
	runOnDisconnect     string
	wg                  *sync.WaitGroup
	externalCmdPool     *externalcmd.Pool
	pathManager         serverPathManager
	parent              *Server

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	mutex     sync.RWMutex
	state     defs.APISRTConnState
	pathName  string
	query     string
	sconn     *srtgo.SrtSocket
}

func (c *conn) initialize() {
	c.ctx, c.ctxCancel = context.WithCancel(c.parentCtx)

	c.created = time.Now()
	c.uuid = uuid.New()
	c.state = defs.APISRTConnStateIdle

	c.Log(logger.Info, "opened")

	c.wg.Add(1)
	go c.run()
}

func (c *conn) Close() {
	c.ctxCancel()
}

// Log implements logger.Writer.
func (c *conn) Log(level logger.Level, format string, args ...interface{}) {
	c.parent.Log(level, "[conn %v] "+format, append([]interface{}{c.connSck.addr.String()}, args...)...)
}

func (c *conn) ip() net.IP {
	return c.connSck.addr.IP
}

func (c *conn) run() { //nolint:dupl
	defer c.wg.Done()

	onDisconnectHook := hooks.OnConnect(hooks.OnConnectParams{
		Logger:              c,
		ExternalCmdPool:     c.externalCmdPool,
		RunOnConnect:        c.runOnConnect,
		RunOnConnectRestart: c.runOnConnectRestart,
		RunOnDisconnect:     c.runOnDisconnect,
		RTSPAddress:         c.rtspAddress,
		Desc:                c.APIReaderDescribe(),
	})
	defer onDisconnectHook()

	err := c.runInner()

	c.ctxCancel()

	c.parent.closeConn(c)

	c.Log(logger.Info, "closed: %v", err)
}

func (c *conn) runInner() error {
	srtStreamID, err := c.connSck.socket.GetSockOptString(srtgo.SRTO_STREAMID)
	if err != nil {
		return fmt.Errorf("cannot get stream ID: %w", err)
	}

	var streamID streamID
	err = streamID.unmarshal(srtStreamID)
	if err != nil {
		c.connSck.socket.SetRejectReason(int(srtgo.EConnRej))
		return fmt.Errorf("invalid stream ID '%s': %w", srtStreamID, err)
	}

	if streamID.mode == streamIDModePublish {
		return c.runPublish(&streamID)
	}
	return c.runRead(&streamID)
}

func (c *conn) runPublish(streamID *streamID) error {
	pathConf, err := c.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:    streamID.path,
			Query:   streamID.query,
			Publish: true,
			Proto:   auth.ProtocolSRT,
			ID:      &c.uuid,
			Credentials: &auth.Credentials{
				User: streamID.user,
				Pass: streamID.pass,
			},
			IP: c.ip(),
		},
	})
	if err != nil {
		var terr *auth.Error
		if errors.As(err, &terr) {
			// wait some seconds to delay brute force attacks
			<-time.After(auth.PauseAfterError)
			c.connSck.socket.SetRejectReason(int(srtgo.EConnRej))
			return terr
		}
		c.connSck.socket.SetRejectReason(int(srtgo.EConnRej))
		return err
	}

	sconn := c.connSck.socket

	readerErr := make(chan error)
	go func() {
		readerErr <- c.runPublishReader(sconn, streamID, pathConf)
	}()

	select {
	case err = <-readerErr:
		sconn.Close()
		return err

	case <-c.ctx.Done():
		sconn.Close()
		<-readerErr
		return errors.New("terminated")
	}
}

func (c *conn) runPublishReader(sconn *srtgo.SrtSocket, streamID *streamID, pathConf *conf.Path) error {
	sconn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
	r := &mpegts.EnhancedReader{R: sconn}
	err := r.Initialize()
	if err != nil {
		return err
	}

	decodeErrors := &counterdumper.CounterDumper{
		OnReport: func(val uint64) {
			c.Log(logger.Warn, "%d decode %s",
				val,
				func() string {
					if val == 1 {
						return "error"
					}
					return "errors"
				}())
		},
	}

	decodeErrors.Start()
	defer decodeErrors.Stop()

	r.OnDecodeError(func(_ error) {
		decodeErrors.Increase()
	})

	var stream *stream.Stream

	medias, err := mpegts.ToStream(r, &stream, c)
	if err != nil {
		return err
	}

	var path defs.Path
	path, stream, err = c.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author:             c,
		Desc:               &description.Session{Medias: medias},
		GenerateRTPPackets: true,
		FillNTP:            true,
		ConfToCompare:      pathConf,
		AccessRequest: defs.PathAccessRequest{
			Name:     streamID.path,
			Query:    streamID.query,
			Publish:  true,
			SkipAuth: true,
		},
	})
	if err != nil {
		return err
	}

	defer path.RemovePublisher(defs.PathRemovePublisherReq{Author: c})

	c.mutex.Lock()
	c.state = defs.APISRTConnStatePublish
	c.pathName = streamID.path
	c.query = streamID.query
	c.sconn = sconn
	c.mutex.Unlock()

	for {
		err = r.Read()
		if err != nil {
			return err
		}
	}
}

func (c *conn) runRead(streamID *streamID) error {
	path, strm, err := c.pathManager.AddReader(defs.PathAddReaderReq{
		Author: c,
		AccessRequest: defs.PathAccessRequest{
			Name:  streamID.path,
			Query: streamID.query,
			Proto: auth.ProtocolSRT,
			ID:    &c.uuid,
			Credentials: &auth.Credentials{
				User: streamID.user,
				Pass: streamID.pass,
			},
			IP: c.ip(),
		},
	})
	if err != nil {
		var terr *auth.Error
		if errors.As(err, &terr) {
			// wait some seconds to delay brute force attacks
			<-time.After(auth.PauseAfterError)
			c.connSck.socket.SetRejectReason(int(srtgo.EConnRej))
			return terr
		}
		c.connSck.socket.SetRejectReason(int(srtgo.EConnRej))
		return err
	}

	defer path.RemoveReader(defs.PathRemoveReaderReq{Author: c})

	sconn := c.connSck.socket

	defer sconn.Close()

	bw := bufio.NewWriterSize(sconn, srtMaxPayloadSize(c.udpMaxPayloadSize))

	r := &stream.Reader{Parent: c}

	err = mpegts.FromStream(strm.Desc, r, bw, sconn, time.Duration(c.writeTimeout))
	if err != nil {
		return err
	}

	c.mutex.Lock()
	c.state = defs.APISRTConnStateRead
	c.pathName = streamID.path
	c.query = streamID.query
	c.sconn = sconn
	c.mutex.Unlock()

	c.Log(logger.Info, "is reading from path '%s', %s",
		path.Name(), defs.FormatsInfo(r.Formats()))

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          c,
		ExternalCmdPool: c.externalCmdPool,
		Conf:            path.SafeConf(),
		ExternalCmdEnv:  path.ExternalCmdEnv(),
		Reader:          c.APIReaderDescribe(),
		Query:           streamID.query,
	})
	defer onUnreadHook()

	// disable read deadline
	sconn.SetReadDeadline(time.Time{})

	strm.AddReader(r)
	defer strm.RemoveReader(r)

	select {
	case <-c.ctx.Done():
		return fmt.Errorf("terminated")

	case err = <-r.Error():
		return err
	}
}

// APIReaderDescribe implements reader.
func (c *conn) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "srtConn",
		ID:   c.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (c *conn) APISourceDescribe() defs.APIPathSourceOrReader {
	return c.APIReaderDescribe()
}

func (c *conn) apiItem() *defs.APISRTConn {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	item := &defs.APISRTConn{
		ID:         c.uuid,
		Created:    c.created,
		RemoteAddr: c.connSck.addr.String(),
		State:      c.state,
		Path:       c.pathName,
		Query:      c.query,
	}

	stats, err := c.connSck.socket.Stats()
	if err != nil {
		return nil
	}

	item.PacketsSent = uint64(stats.PktSentTotal)
	item.PacketsReceived = uint64(stats.PktRecvTotal)
	// item.PacketsSentUnique = uint64(stats.PktSentTotal - int64(stats.PktRetransTotal))
	// item.PacketsReceivedUnique = uint64(stats.PktRecvTotal - int64(stats.PktRetransTotal))
	item.PacketsSendLoss = uint64(stats.PktSndLossTotal)
	item.PacketsReceivedLoss = uint64(stats.PktRcvLossTotal)
	item.PacketsRetrans = uint64(stats.PktRetransTotal)
	// item.PacketsReceivedRetrans = s.Accumulated.PktRecvRetrans
	item.PacketsSentACK = uint64(stats.PktSentACKTotal)
	item.PacketsReceivedACK = uint64(stats.PktRecvACKTotal)
	item.PacketsSentNAK = uint64(stats.PktSentNAKTotal)
	item.PacketsReceivedNAK = uint64(stats.PktRecvNAKTotal)
	// item.PacketsSentKM = s.Accumulated.PktSentKM
	// item.PacketsReceivedKM = s.Accumulated.PktRecvKM
	item.UsSndDuration = uint64(stats.UsSndDurationTotal)
	// item.PacketsReceivedBelated = s.Accumulated.PktRecvBelated
	item.PacketsSendDrop = uint64(stats.PktSndDropTotal)
	item.PacketsReceivedDrop = uint64(stats.PktRcvDropTotal)
	item.PacketsReceivedUndecrypt = uint64(stats.PktRcvUndecryptTotal)
	item.BytesSent = uint64(stats.ByteSentTotal)
	item.BytesReceived = uint64(stats.ByteRecvTotal)
	// item.BytesSentUnique = s.Accumulated.BytesSentUnique
	// item.BytesReceivedUnique = s.Accumulated.ByteRecvUnique
	item.BytesReceivedLoss = uint64(stats.ByteRcvLossTotal)
	item.BytesRetrans = uint64(stats.ByteRetransTotal)
	// item.BytesReceivedRetrans = s.Accumulated.ByteRecvRetrans
	// item.BytesReceivedBelated = s.Accumulated.ByteRecvBelated
	item.BytesSendDrop = uint64(stats.ByteSndDropTotal)
	item.BytesReceivedDrop = uint64(stats.ByteRcvDropTotal)
	item.BytesReceivedUndecrypt = uint64(stats.ByteRcvUndecryptTotal)
	item.UsPacketsSendPeriod = stats.UsPktSndPeriod
	item.PacketsFlowWindow = uint64(stats.PktFlowWindow)
	item.PacketsFlightSize = uint64(stats.PktFlightSize)
	item.MsRTT = stats.MsRTT
	item.MbpsSendRate = stats.MbpsSendRate
	item.MbpsReceiveRate = stats.MbpsRecvRate
	// item.MbpsLinkCapacity = s.Instantaneous.MbpsLinkCapacity
	item.BytesAvailSendBuf = uint64(stats.ByteAvailSndBuf)
	item.BytesAvailReceiveBuf = uint64(stats.ByteAvailRcvBuf)
	item.MbpsMaxBW = stats.MbpsMaxBW
	item.ByteMSS = uint64(stats.ByteMSS)
	item.PacketsSendBuf = uint64(stats.PktSndBuf)
	item.BytesSendBuf = uint64(stats.ByteSndBuf)
	item.MsSendBuf = uint64(stats.MsSndBuf)
	item.MsSendTsbPdDelay = uint64(stats.MsSndTsbPdDelay)
	item.PacketsReceiveBuf = uint64(stats.PktRcvBuf)
	item.BytesReceiveBuf = uint64(stats.ByteRcvBuf)
	item.MsReceiveBuf = uint64(stats.MsRcvBuf)
	item.MsReceiveTsbPdDelay = uint64(stats.MsRcvTsbPdDelay)
	item.PacketsReorderTolerance = uint64(stats.PktReorderTolerance)
	item.PacketsReceivedAvgBelatedTime = uint64(stats.PktRcvAvgBelatedTime)
	// item.PacketsSendLossRate = s.Instantaneous.PktSendLossRate
	// item.PacketsReceivedLossRate = s.Instantaneous.PktRecvLossRate

	return item
}
