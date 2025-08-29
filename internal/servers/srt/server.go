// Package srt contains a SRT server.
package srt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/haivision/srtgo"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// ErrConnNotFound is returned when a connection is not found.
var ErrConnNotFound = errors.New("connection not found")

func interfaceIsEmpty(i interface{}) bool {
	return reflect.ValueOf(i).Kind() != reflect.Ptr || reflect.ValueOf(i).IsNil()
}

func srtMaxPayloadSize(u int) int {
	return ((u - 16) / 188) * 188 // 16 = SRT header, 188 = MPEG-TS packet
}

type serverAPIConnsListRes struct {
	data *defs.APISRTConnList
	err  error
}

type serverAPIConnsListReq struct {
	res chan serverAPIConnsListRes
}

type serverAPIConnsGetRes struct {
	data *defs.APISRTConn
	err  error
}

type serverAPIConnsGetReq struct {
	uuid uuid.UUID
	res  chan serverAPIConnsGetRes
}

type serverAPIConnsKickRes struct {
	err error
}

type serverAPIConnsKickReq struct {
	uuid uuid.UUID
	res  chan serverAPIConnsKickRes
}

type serverMetrics interface {
	SetSRTServer(defs.APISRTServer)
}

type serverPathManager interface {
	FindPathConf(req defs.PathFindPathConfReq) (*conf.Path, error)
	AddPublisher(req defs.PathAddPublisherReq) (defs.Path, *stream.Stream, error)
	AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error)
}

type serverParent interface {
	logger.Writer
}

// Server is a SRT server.
type Server struct {
	Address             string
	RTSPAddress         string
	ReadTimeout         conf.Duration
	WriteTimeout        conf.Duration
	UDPMaxPayloadSize   int
	RunOnConnect        string
	RunOnConnectRestart bool
	RunOnDisconnect     string
	ExternalCmdPool     *externalcmd.Pool
	Metrics             serverMetrics
	PathManager         serverPathManager
	Parent              serverParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup
	conns     map[*conn]struct{}

	// in
	chNewConnRequest chan struct {
		socket *srtgo.SrtSocket
		addr   *net.UDPAddr
	}
	chAcceptErr    chan error
	chCloseConn    chan *conn
	chAPIConnsList chan serverAPIConnsListReq
	chAPIConnsGet  chan serverAPIConnsGetReq
	chAPIConnsKick chan serverAPIConnsKickReq

	sck *srtgo.SrtSocket
}

func listenCallback(s *Server, socket *srtgo.SrtSocket, _ int, _ *net.UDPAddr, streamid string) bool {
	var streamID streamID
	err := streamID.unmarshal(streamid)
	if err != nil {
		socket.SetRejectReason(int(srtgo.EConnRej))
		s.Log(logger.Error, "invalid stream ID '%s': %v", streamid, err)
		return false
	}

	req := defs.PathAccessRequest{
		Name:    streamID.path,
		Publish: streamID.mode == streamIDModePublish,
		Query:   streamID.query,
		Credentials: &auth.Credentials{
			User: streamID.user,
			Pass: streamID.pass,
		},
	}
	path, err := s.PathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: req,
	})
	if err != nil {
		return false
	}

	var passphrase string

	if streamID.mode == streamIDModePublish {
		passphrase = path.SRTPublishPassphrase
	} else {
		passphrase = path.SRTReadPassphrase
	}

	if passphrase == "" {
		return true
	}

	err = socket.SetSockOptString(srtgo.SRTO_PASSPHRASE, passphrase)
	if err != nil {
		s.Log(logger.Error, "invalid passphrase in config")
		return false
	}

	// allow connection
	return true
}

// Initialize initializes the server.
func (s *Server) Initialize() error {
	options := make(map[string]string)
	options["blocking"] = "0"
	options["conntimeo"] = strconv.Itoa(int(time.Duration(s.ReadTimeout).Milliseconds()))
	options["payloadsize"] = strconv.Itoa(srtMaxPayloadSize(s.UDPMaxPayloadSize))

	host, portStr, err := net.SplitHostPort(s.Address)
	if err != nil {
		return err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil {
		return err
	}
	port := uint16(portInt)

	s.sck = srtgo.NewSrtSocket(host, port, options)
	s.sck.SetListenCallback(func(socket *srtgo.SrtSocket, version int, addr *net.UDPAddr, streamid string) bool {
		return listenCallback(s, socket, version, addr, streamid)
	})
	err = s.sck.Listen(1)
	if err != nil {
		return err
	}

	s.ctx, s.ctxCancel = context.WithCancel(context.Background())

	s.conns = make(map[*conn]struct{})
	s.chNewConnRequest = make(chan struct {
		socket *srtgo.SrtSocket
		addr   *net.UDPAddr
	})
	s.chAcceptErr = make(chan error)
	s.chCloseConn = make(chan *conn)
	s.chAPIConnsList = make(chan serverAPIConnsListReq)
	s.chAPIConnsGet = make(chan serverAPIConnsGetReq)
	s.chAPIConnsKick = make(chan serverAPIConnsKickReq)

	s.Log(logger.Info, "listener opened on "+s.Address+" (UDP)")

	l := &listener{
		sck:    s.sck,
		wg:     &s.wg,
		parent: s,
	}
	l.initialize()

	s.wg.Add(1)
	go s.run()

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetSRTServer(s)
	}

	return nil
}

// Log implements logger.Writer.
func (s *Server) Log(level logger.Level, format string, args ...interface{}) {
	s.Parent.Log(level, "[SRT] "+format, args...)
}

// Close closes the server.
func (s *Server) Close() {
	s.Log(logger.Info, "listener is closing")

	if !interfaceIsEmpty(s.Metrics) {
		s.Metrics.SetSRTServer(nil)
	}

	s.ctxCancel()
	s.wg.Wait()
}

func (s *Server) run() {
	defer s.wg.Done()

outer:
	for {
		select {
		case err := <-s.chAcceptErr:
			s.Log(logger.Error, "%s", err)
			break outer

		case req := <-s.chNewConnRequest:
			c := &conn{
				parentCtx:           s.ctx,
				rtspAddress:         s.RTSPAddress,
				readTimeout:         s.ReadTimeout,
				writeTimeout:        s.WriteTimeout,
				udpMaxPayloadSize:   s.UDPMaxPayloadSize,
				connSck:             req,
				runOnConnect:        s.RunOnConnect,
				runOnConnectRestart: s.RunOnConnectRestart,
				runOnDisconnect:     s.RunOnDisconnect,
				wg:                  &s.wg,
				externalCmdPool:     s.ExternalCmdPool,
				pathManager:         s.PathManager,
				parent:              s,
			}
			c.initialize()
			s.conns[c] = struct{}{}

		case c := <-s.chCloseConn:
			delete(s.conns, c)

		case req := <-s.chAPIConnsList:
			data := &defs.APISRTConnList{
				Items: []*defs.APISRTConn{},
			}

			for c := range s.conns {
				data.Items = append(data.Items, c.apiItem())
			}

			sort.Slice(data.Items, func(i, j int) bool {
				return data.Items[i].Created.Before(data.Items[j].Created)
			})

			req.res <- serverAPIConnsListRes{data: data}

		case req := <-s.chAPIConnsGet:
			c := s.findConnByUUID(req.uuid)
			if c == nil {
				req.res <- serverAPIConnsGetRes{err: ErrConnNotFound}
				continue
			}

			req.res <- serverAPIConnsGetRes{data: c.apiItem()}

		case req := <-s.chAPIConnsKick:
			c := s.findConnByUUID(req.uuid)
			if c == nil {
				req.res <- serverAPIConnsKickRes{err: ErrConnNotFound}
				continue
			}

			delete(s.conns, c)
			c.Close()
			req.res <- serverAPIConnsKickRes{}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	s.sck.Close()
}

func (s *Server) findConnByUUID(uuid uuid.UUID) *conn {
	for sx := range s.conns {
		if sx.uuid == uuid {
			return sx
		}
	}
	return nil
}

// newConnRequest is called by srtListener.
func (s *Server) newConnRequest(connSck *srtgo.SrtSocket, udpAddr *net.UDPAddr) {
	select {
	case s.chNewConnRequest <- struct {
		socket *srtgo.SrtSocket
		addr   *net.UDPAddr
	}{connSck, udpAddr}:
	case <-s.ctx.Done():
		connSck.SetRejectReason(int(srtgo.ESClosed))
	}
}

// acceptError is called by srtListener.
func (s *Server) acceptError(err error) {
	select {
	case s.chAcceptErr <- err:
	case <-s.ctx.Done():
	}
}

// closeConn is called by conn.
func (s *Server) closeConn(c *conn) {
	select {
	case s.chCloseConn <- c:
	case <-s.ctx.Done():
	}
}

// APIConnsList is called by api.
func (s *Server) APIConnsList() (*defs.APISRTConnList, error) {
	req := serverAPIConnsListReq{
		res: make(chan serverAPIConnsListRes),
	}

	select {
	case s.chAPIConnsList <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APIConnsGet is called by api.
func (s *Server) APIConnsGet(uuid uuid.UUID) (*defs.APISRTConn, error) {
	req := serverAPIConnsGetReq{
		uuid: uuid,
		res:  make(chan serverAPIConnsGetRes),
	}

	select {
	case s.chAPIConnsGet <- req:
		res := <-req.res
		return res.data, res.err

	case <-s.ctx.Done():
		return nil, fmt.Errorf("terminated")
	}
}

// APIConnsKick is called by api.
func (s *Server) APIConnsKick(uuid uuid.UUID) error {
	req := serverAPIConnsKickReq{
		uuid: uuid,
		res:  make(chan serverAPIConnsKickRes),
	}

	select {
	case s.chAPIConnsKick <- req:
		res := <-req.res
		return res.err

	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}
}
