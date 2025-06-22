package srt

import (
	"net"
	"sync"
	"time"

	"github.com/haivision/srtgo"
)

type listener struct {
	sck    *srtgo.SrtSocket
	wg     *sync.WaitGroup
	parent *Server
}

func (l *listener) initialize() {
	l.wg.Add(1)
	go l.run()
}

func (l *listener) run() {
	defer l.wg.Done()

	err := l.runInner()

	l.parent.acceptError(err)
}

func (l *listener) runInner() error {
	for {
		// Accept connections with a timeout to avoid blocking indefinitely.
		l.sck.SetPollTimeout(time.Second)
		l.sck.SetReadDeadline(time.Now().Add(time.Second))

		s, u, err := l.sck.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}

		l.parent.newConnRequest(s, u)
	}
}
